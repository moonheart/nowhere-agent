package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/audit"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/oidc"
	"nowhere-agent/internal/settings"
)

// wire_identity.go — the auth surface: identity store/service/handler, the
// credential sweep, the shared audit logger, OIDC SSO, phone SMS-OTP, email
// password reset, and the platform-admin bootstrap. All extracted verbatim
// from run() (see deps.go for why the phases exist).

func (d *serverDeps) wireIdentity(ctx context.Context) error {
	cfg, log, mux := d.cfg, d.log, d.mux

	// First-account admin bootstrap (legacy) is enabled only when the
	// deployment explicitly opted in via BOOTSTRAP_ADMIN_EMAIL: on a public
	// deployment the first random signup must not claim the admin role before
	// operations can.
	d.identityStore = identity.NewStore(d.pool).WithFirstAccountAdmin(cfg.Identity.BootstrapAdminEmail != "")
	d.identitySvc = identity.NewService(d.identityStore)

	// Credential reaper: expired session tokens, phone OTPs, and service keys
	// are rejected by every auth path, so their rows are garbage; an hourly
	// pass deletes credentials dead for more than a day (the grace keeps a
	// just-expired token's audit trail). Revoked service keys are deliberately
	// kept — the admin console's revoked list shows them. Best-effort: a
	// failed pass is logged and retried next hour.
	hourlySweep(ctx, log, "identity credential", func() error {
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		removed, err := d.identityStore.SweepExpired(ctx, cutoff)
		if err != nil {
			return err
		}
		if removed > 0 {
			log.Info("identity credential sweep removed rows", "count", removed)
		}
		return nil
	})

	d.identityHandler = identity.NewHandler(d.identitySvc).WithThrottle(identity.NewLoginThrottler()).WithTOTPThrottle(identity.NewLoginThrottler()).WithSignupThrottle(identity.NewLoginThrottler())
	d.identityHandler.Register(mux)

	// Audit trail (enterprise-readiness P0): one append-only logger shared by the
	// identity handler (auth events) and the admin console (administrative and
	// credential actions). Recording is best-effort — a broken sink must never
	// take a login or an admin action down — so it is wired as an option, not a
	// hard dependency, and write failures surface only in the server log.
	d.auditLogger = audit.NewLogger(d.pool, log)
	d.identityHandler.WithAudit(d.auditLogger)

	// Single-sign-on (enterprise-readiness P1-2): when OIDC_ISSUER is set, mount
	// the authorization-code flow so users sign in via the enterprise IdP (钉钉 /
	// 企业微信 / 飞书 / any standard OIDC provider) instead of a platform
	// password. SSO is only a sign-in MECHANISM — it provisions/resolves the
	// platform account (user_identities links issuer+subject) and issues the
	// platform's own bearer token, so every downstream concern (RequireAuth,
	// teams, quotas) is unchanged. A misconfigured issuer fails the boot: better
	// to refuse to start than to offer a broken SSO button.
	if cfg.OIDC.Enabled() {
		oidcProvider, err := oidc.NewProvider(ctx, oidc.Config{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			RedirectURL:  cfg.OIDC.RedirectURL,
			Scopes:       strings.Split(cfg.OIDC.Scopes, " "),
			PKCE:         cfg.OIDC.PKCE,
		}, nil)
		if err != nil {
			return fmt.Errorf("oidc sso: %w", err)
		}
		oidcHandler := oidc.NewHandler(oidcProvider, d.identityStore,
			func(ctx context.Context, u identity.User) (string, error) {
				return d.identitySvc.IssueToken(ctx, u)
			}).
			// Secure state cookie regardless of the gateway's own connection
			// scheme: TLS terminates at the reverse proxy, so r.TLS is never
			// set and the cookie must not depend on it.
			WithSecureCookies(cfg.HTTP.CookieSecure).
			// MFA parity: SSO logins respect the account's TOTP second factor
			// exactly like password logins — a TOTP-enabled account gets a
			// challenge instead of a token, so the IdP cannot bypass it.
			WithTotpChallenge(func(ctx context.Context, u identity.User) (string, error) {
				enabled, err := d.identitySvc.TOTPEnabled(ctx, u.ID)
				if err != nil || !enabled {
					return "", err
				}
				ch, err := d.identitySvc.BeginTOTPChallenge(ctx, u)
				if err != nil {
					return "", err
				}
				return ch.Token, nil
			}).
			WithAudit(d.auditLogger)
		oidcHandler.Register(mux)
		mux.Handle("GET /auth/oidc/enabled", oidc.EnabledProbe())
		log.Info("oidc sso enabled", "issuer", cfg.OIDC.Issuer, "redirect", cfg.OIDC.RedirectURL, "pkce", cfg.OIDC.PKCE)
		// Always surface the state-cookie policy: the default (Secure) is
		// silently catastrophic on a plain-HTTP deployment — the browser
		// never sends the cookie and SSO fails with zero requests hitting
		// the server — so it must be visible in the boot log, not just the
		// inverted env override.
		log.Info("oidc sso cookie", "secure", cfg.HTTP.CookieSecure)
		if !cfg.HTTP.CookieSecure {
			log.Warn("oidc state cookie not Secure: HTTP_COOKIE_SECURE=false ships it over plain HTTP — intended for local/dev only", "issuer", cfg.OIDC.Issuer)
		}
	}

	// Phone + SMS-OTP authentication (domestic enterprise account convention):
	// users register/sign in with a mobile number + one-time code. The SMS
	// gateway is deployment-owned (the platform POSTs {phone, code} to the
	// URL); "log://" prints codes to the server log for dev/self-host.
	// Verification provisions/resolves the account and issues the platform's
	// own bearer token — downstream (RequireAuth/teams/quotas) is unchanged.
	// The channel resolves from the RUNTIME settings on every send
	// (phone_sms_url, phone_sms_timeout), so the admin console switches
	// gateways or disables phone login without a restart; the boot env value
	// is still validated once (a malformed boot URL fails the boot).
	if url := cfg.Phone.SMSURL; url != "" && url != "log://" &&
		!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("phone sms: PHONE_SMS_URL must be an http(s) URL or log://, got %q", url)
	}
	// One OTP throttler shared by every phone-verified route (login verify,
	// password reset, profile binding): a locked (phone, ip) pair stays locked
	// across all of them.
	d.phoneThrottle = identity.NewOTPThrottler()
	phoneHandler := identity.NewPhoneHandler(d.identitySvc, identity.NewRuntimeSMSProvider(
		func() string { return d.settings.String(settings.KeyPhoneSMSURL) },
		func() time.Duration { return d.settings.Duration(settings.KeyPhoneSMSTimeout) },
		log)).WithAudit(d.auditLogger).
		WithEnabledFunc(func() bool { return d.settings.String(settings.KeyPhoneSMSURL) != "" }).
		WithThrottle(d.phoneThrottle)
	phoneHandler.SetLogger(log)
	phoneHandler.Register(mux)
	if cfg.Phone.Enabled() {
		log.Info("phone OTP auth enabled (POST /api/auth/phone/*)", "channel", cfg.Phone.SMSURL)
	}

	// Email self-service password recovery: POST /api/auth/email/reset-code +
	// reset-password, the recovery path for deployments without the phone
	// channel. The platform has NO mail channel today, so codes are printed to
	// the server log (the dev/self-host path, mirroring the phone channel's
	// log:// mode) via a provider a deployment with SMTP can swap. The same
	// shared OTP throttler guards both channels (keys never collide: emails
	// contain "@", phones are 11 digits).
	emailResetHandler := identity.NewEmailResetHandler(d.identitySvc,
		identity.NewLogEmailResetCodeProvider(log)).
		WithAudit(d.auditLogger).
		WithThrottle(d.phoneThrottle)
	emailResetHandler.SetLogger(log)
	emailResetHandler.Register(mux)
	log.Info("email password reset enabled (POST /api/auth/email/*)", "channel", "log://")

	// Platform-admin bootstrap (admin-console): the first account to sign up on
	// an empty database is made an admin automatically, which does nothing for a
	// deployment whose accounts predate the role. BOOTSTRAP_ADMIN_EMAIL names
	// one to promote; it is idempotent, so it can stay set, and it is the
	// recovery path if no admin remains. An email nobody holds is a warning, not
	// a boot failure — a stale value must not keep the server down.
	if email := cfg.Identity.BootstrapAdminEmail; email != "" {
		switch found, err := d.identitySvc.PromoteByEmail(ctx, email); {
		case err != nil:
			log.Warn("bootstrap admin promotion failed", "email", email, "err", err)
		case found:
			log.Info("bootstrap admin ensured", "email", email)
		default:
			log.Warn("bootstrap admin email matches no account", "email", email)
		}
	}
	return nil
}

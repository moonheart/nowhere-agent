// Command migrate applies database migrations.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"nowhere-agent/internal/config"
	"nowhere-agent/internal/secrets"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	seed := flag.Bool("seed-from-env", false, "import LLM_*/VISION_* into the provider registry once (only when empty)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return err
	}

	m, err := migrate.NewWithDatabaseInstance("file://migrations", "postgres", driver)
	if err != nil {
		return err
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return err
	}
	fmt.Println("migrations applied")

	if *seed {
		if err := seedProvidersFromEnv(db, cfg.Secrets.MasterKey); err != nil {
			return fmt.Errorf("seed providers from env: %w", err)
		}
	}
	return nil
}

// seedProvidersFromEnv is the one-time migration of the env-based model
// configuration into the DB registry. It runs only when the providers table is
// empty, so it never clobbers console-managed providers on a later run. System
// providers are created from LLM_* (marked the platform default) and VISION_*
// (a vision-capable provider). Keys are encrypted at rest when a master key is
// configured.
func seedProvidersFromEnv(db *sql.DB, masterKey string) error {
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM providers`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		fmt.Println("providers already present; skipping env seed")
		return nil
	}

	var enc *secrets.Encryptor
	if masterKey != "" {
		var err error
		enc, err = secrets.NewSingle([]byte(masterKey))
		if err != nil {
			return err
		}
	}
	encrypt := func(plain string) (string, error) {
		if enc == nil {
			return plain, nil
		}
		return enc.Encrypt(plain)
	}

	provider := strings.TrimSpace(os.Getenv("LLM_PROVIDER"))
	model := strings.TrimSpace(os.Getenv("LLM_MODEL"))
	if provider != "" && model != "" {
		id, err := insertProvider(db, provider, provider, os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), true, encrypt)
		if err != nil {
			return err
		}
		if err := insertModel(db, id, model, "", false, true, encrypt); err != nil {
			return err
		}
		fmt.Printf("seeded provider %q (model %q) from LLM_*\n", provider, model)
	}

	visionProvider := strings.TrimSpace(os.Getenv("VISION_PROVIDER"))
	visionModel := strings.TrimSpace(os.Getenv("VISION_MODEL"))
	if visionProvider != "" && visionModel != "" {
		name := visionProvider
		if name == provider {
			name += "-vision"
		}
		id, err := insertProvider(db, name, visionProvider, os.Getenv("VISION_BASE_URL"), os.Getenv("VISION_API_KEY"), false, encrypt)
		if err != nil {
			return err
		}
		if err := insertModel(db, id, visionModel, "", true, true, encrypt); err != nil {
			return err
		}
		fmt.Printf("seeded vision provider %q (model %q) from VISION_*\n", name, visionModel)
	}

	if provider == "" || model == "" {
		fmt.Println("LLM_PROVIDER/LLM_MODEL unset; nothing to seed")
	}
	return nil
}

func insertProvider(db *sql.DB, name, vendor, baseURL, apiKey string, isDefault bool, encrypt func(string) (string, error)) (string, error) {
	stored, err := encrypt(apiKey)
	if err != nil {
		return "", err
	}
	var id string
	err = db.QueryRow(`
		INSERT INTO providers (scope, name, vendor, base_url, api_key, is_default)
		VALUES ('system', $1, $2, $3, $4, $5)
		RETURNING id`, name, vendor, baseURL, stored, isDefault).Scan(&id)
	return id, err
}

func insertModel(db *sql.DB, providerID, name, displayName string, vision, isDefault bool, encrypt func(string) (string, error)) error {
	_, err := db.Exec(`
		INSERT INTO provider_models (provider_id, name, display_name, vision, is_default)
		VALUES ($1, $2, $3, $4, $5)`, providerID, name, displayName, vision, isDefault)
	return err
}

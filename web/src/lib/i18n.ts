// Minimal UI localization for the user-facing console surfaces (login, chat,
// session list). The platform targets Chinese enterprises first, so the
// language follows the browser locale: zh-* users get Chinese, everyone else
// keeps English. This is deliberately a small typed dictionary — not a full
// i18n framework — so the most visible screens can be localized without
// churning every component in the admin console.
//
// Add keys here as more surfaces are localized; keep the key list typed so a
// missing translation is a compile error, not a runtime "undefined".

export type I18nKey =
  | "login.title"
  | "login.titleSignup"
  | "login.subtitle"
  | "login.subtitleSignup"
  | "login.sso"
  | "login.orEmail"
  | "login.email"
  | "login.password"
  | "login.busy"
  | "login.submit"
  | "login.submitSignup"
  | "login.toggleToSignup"
  | "login.toggleToLogin"
  | "chat.new"
  | "chat.help"
  | "chat.placeholder"
  | "chat.send"
  | "chat.scrollBottom"
  | "chat.attachImage"
  | "chat.removeImage"
  | "chat.stop"
  | "chat.loading"
  | "chat.error"
  | "profile.exportData"
  | "login.phone"
  | "login.phoneSubtitle"
  | "login.sendCode"
  | "login.resendIn"
  | "login.code"
  | "login.backToEmail";

const zh: Record<I18nKey, string> = {
  "login.title": "登录",
  "login.titleSignup": "创建账号",
  "login.subtitle": "继续使用 nowhere-agent。",
  "login.subtitleSignup": "注册一个新的 nowhere-agent 账号。",
  "login.sso": "使用 SSO 登录",
  "login.orEmail": "或使用邮箱",
  "login.email": "邮箱",
  "login.password": "密码",
  "login.busy": "处理中…",
  "login.submit": "登录",
  "login.submitSignup": "注册",
  "login.toggleToSignup": "没有账号?去注册",
  "login.toggleToLogin": "已有账号?去登录",
  "chat.new": "新建对话",
  "chat.help": "有什么可以帮你?",
  "chat.placeholder": "给 nowhere-agent 发消息…",
  "chat.send": "发送",
  "chat.scrollBottom": "滚动到底部",
  "chat.attachImage": "附加图片",
  "chat.removeImage": "移除图片",
  "chat.stop": "停止",
  "chat.loading": "加载中…",
  "chat.error": "出错了",
  "profile.exportData": "导出我的数据",
  "login.phone": "手机号",
  "login.phoneSubtitle": "使用手机号 + 验证码登录或注册。",
  "login.sendCode": "发送验证码",
  "login.resendIn": "重新发送",
  "login.code": "验证码",
  "login.backToEmail": "返回邮箱登录",
};

const en: Record<I18nKey, string> = {
  "login.title": "Sign in",
  "login.titleSignup": "Create account",
  "login.subtitle": "Continue to nowhere-agent.",
  "login.subtitleSignup": "Set up a new nowhere-agent account.",
  "login.sso": "Sign in with SSO",
  "login.orEmail": "or with email",
  "login.email": "Email",
  "login.password": "Password",
  "login.busy": "Working…",
  "login.submit": "Sign in",
  "login.submitSignup": "Sign up",
  "login.toggleToSignup": "No account? Sign up",
  "login.toggleToLogin": "Have an account? Sign in",
  "chat.new": "New chat",
  "chat.help": "How can I help?",
  "chat.placeholder": "Message nowhere-agent…",
  "chat.send": "Send",
  "chat.scrollBottom": "Scroll to bottom",
  "chat.attachImage": "Attach image",
  "chat.removeImage": "Remove image",
  "chat.stop": "Stop",
  "chat.loading": "Loading…",
  "chat.error": "Something went wrong",
  "profile.exportData": "Export my data",
  "login.phone": "Phone",
  "login.phoneSubtitle": "Sign in or register with a mobile number and one-time code.",
  "login.sendCode": "Send code",
  "login.resendIn": "Resend in",
  "login.code": "Verification code",
  "login.backToEmail": "Back to email sign-in",
};

// lang resolves the UI language from the browser locale once (zh-* → zh).
const lang: "zh" | "en" = navigator.language.toLowerCase().startsWith("zh")
  ? "zh"
  : "en";

// t returns the localized string for key.
export function t(key: I18nKey): string {
  const dict = lang === "zh" ? zh : en;
  return dict[key];
}

// isZh reports whether the UI is currently rendering Chinese, for the rare
// components that need to branch beyond plain strings.
export function isZh(): boolean {
  return lang === "zh";
}

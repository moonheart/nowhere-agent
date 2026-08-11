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
  | "login.backToEmail"
  | "admin.sectionAccount"
  | "admin.sectionTeams"
  | "admin.sectionPlatform"
  | "admin.profile"
  | "admin.myUsage"
  | "admin.myMemories"
  | "admin.mySkills"
  | "admin.myAgents"
  | "admin.scheduledTasks"
  | "admin.allMyTeams"
  | "admin.users"
  | "admin.teams"
  | "admin.usage"
  | "admin.quotas"
  | "admin.providers"
  | "admin.memories"
  | "admin.skills"
  | "admin.agents"
  | "admin.auditTrail"
  | "admin.backToChat";

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
  "admin.sectionAccount": "我的账户",
  "admin.sectionTeams": "团队",
  "admin.sectionPlatform": "平台",
  "admin.profile": "个人资料",
  "admin.myUsage": "我的用量",
  "admin.myMemories": "我的记忆",
  "admin.mySkills": "我的技能",
  "admin.myAgents": "我的代理",
  "admin.scheduledTasks": "定时任务",
  "admin.allMyTeams": "我的全部团队",
  "admin.users": "用户",
  "admin.teams": "团队",
  "admin.usage": "用量",
  "admin.quotas": "配额",
  "admin.providers": "模型服务商",
  "admin.memories": "记忆",
  "admin.skills": "技能",
  "admin.agents": "代理定义",
  "admin.auditTrail": "审计日志",
  "admin.backToChat": "返回聊天",
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
  "admin.sectionAccount": "Account",
  "admin.sectionTeams": "Teams",
  "admin.sectionPlatform": "Platform",
  "admin.profile": "Profile",
  "admin.myUsage": "My usage",
  "admin.myMemories": "My memories",
  "admin.mySkills": "My skills",
  "admin.myAgents": "My agents",
  "admin.scheduledTasks": "Scheduled tasks",
  "admin.allMyTeams": "All my teams",
  "admin.users": "Users",
  "admin.teams": "Teams",
  "admin.usage": "Usage",
  "admin.quotas": "Quotas",
  "admin.providers": "Providers",
  "admin.memories": "Memories",
  "admin.skills": "Skills",
  "admin.agents": "Agents",
  "admin.auditTrail": "Audit trail",
  "admin.backToChat": "Back to chat",
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

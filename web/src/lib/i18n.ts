// Minimal UI localization for the user-facing console surfaces (login, chat,
// session list). The platform targets Chinese enterprises first, so the
// language follows the browser locale: zh-* users get Chinese, everyone else
// keeps English. A localStorage override (nowhere.lang, via setLang) wins over
// the browser locale so a user can switch the UI language without changing the
// OS/browser settings. This is deliberately a small typed dictionary — not a
// full i18n framework — so the most visible screens can be localized without
// churning every component in the admin console.
//
// Add keys here as more surfaces are localized; keep the key list typed so a
// missing translation is a compile error, not a runtime "undefined".

import { useSyncExternalStore } from "react";

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
  | "chat.emptyState"
  | "chat.rerunTitle"
  | "chat.retry"
  | "chat.disclaimer"
  | "chat.searchChats"
  | "chat.loadError"
  | "chat.noConversations"
  | "chat.noConversationsHint"
  | "chat.noMatches"
  | "chat.noMatchesHint"
  | "chat.deleteConversation"
  | "chat.deleteConfirm"
  | "chat.deleteFailed"
  | "chat.untitled"
  | "chat.searching"
  | "chat.loadingMore"
  | "chat.webpImage"
  | "approval.approveRunning"
  | "approval.approve"
  | "approval.approving"
  | "approval.deny"
  | "approval.denying"
  | "approval.waitingEarlier"
  | "clientTool.failedInBrowser"
  | "clientTool.runningInBrowser"
  | "clientTool.retry"
  | "clientTool.retrying"
  | "ask.submit"
  | "ask.sending"
  | "ask.skip"
  | "ask.customPlaceholder"
  | "ask.customAria"
  | "profile.exportData"
  | "login.phone"
  | "login.phoneSubtitle"
  | "login.sendCode"
  | "login.resendIn"
  | "login.code"
  | "login.backToEmail"
  | "login.totpHint"
  | "login.totpCode"
  | "login.forgotPassword"
  | "login.forgotHint"
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
  | "admin.settings"
  | "admin.backToChat"
  | "lang.switchToZh"
  | "lang.switchToEn"
  | "settingsPage.title"
  | "settingsPage.description"
  | "settingsPage.group.tools"
  | "settingsPage.group.webhooks"
  | "settingsPage.group.llm"
  | "settingsPage.group.sandbox"
  | "settingsPage.group.permissions"
  | "settingsPage.group.redaction"
  | "settingsPage.group.subagents"
  | "settingsPage.group.background"
  | "settingsPage.group.http"
  | "settingsPage.group.auth"
  | "settingsPage.group.integrations"
  | "settingsPage.clearOverride"
  | "settingsPage.permissionTitle"
  | "settingsPage.permissionDescription"
  | "settingsPage.currentValue"
  | "settingsPage.save"
  | "settingsPage.saving"
  | "settingsPage.applied"
  | "settingsPage.nothingToChange"
  | "settingsPage.loading"
  | "settingsPage.notSet"
  | "settingsPage.secretSet"
  | "usersPage.title"
  | "usersPage.description"
  | "usersPage.searchPlaceholder"
  | "usersPage.search"
  | "usersPage.loading"
  | "usersPage.colAccount"
  | "usersPage.colRole"
  | "usersPage.colEnabled"
  | "usersPage.colJoined"
  | "usersPage.noAccounts"
  | "usersPage.range"
  | "usersPage.previous"
  | "usersPage.next"
  | "usersPage.you"
  | "usersPage.roleUser"
  | "usersPage.roleAdmin"
  | "usersPage.roleAria"
  | "usersPage.enableAria"
  | "usersPage.deleteAccount"
  | "usersPage.deleteTitle"
  | "usersPage.deleteDescription"
  | "usersPage.resetTitle"
  | "usersPage.resetDescription"
  | "usersPage.newPassword"
  | "usersPage.minLength"
  | "usersPage.reset"
  | "usersPage.newAccount"
  | "usersPage.createTitle"
  | "usersPage.createDescription"
  | "usersPage.email"
  | "usersPage.displayName"
  | "usersPage.namePlaceholder"
  | "usersPage.initialPassword"
  | "usersPage.creating"
  | "usersPage.create";

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
  "chat.emptyState": "随便问点什么,或让我处理你工作区里的文件。",
  "chat.rerunTitle": "重新运行上一条消息",
  "chat.retry": "重试",
  "chat.disclaimer": "nowhere-agent 可以读写你工作区中的文件。重要输出请仔细核对。",
  "chat.searchChats": "搜索对话",
  "chat.loadError": "无法加载会话列表。",
  "chat.noConversations": "还没有对话",
  "chat.noConversationsHint": "点击「新建对话」开始。",
  "chat.noMatches": "没有匹配结果",
  "chat.noMatchesHint": "没有与「{term}」匹配的内容。",
  "chat.deleteConversation": "删除对话",
  "chat.deleteConfirm": "删除这个对话？此操作不可撤销。",
  "chat.deleteFailed": "无法删除对话，请重试。",
  "chat.untitled": "未命名",
  "chat.searching": "搜索中…",
  "chat.loadingMore": "加载中…",
  "chat.webpImage": "WebP 图片",
  "approval.approveRunning": "批准运行 {tool}？",
  "approval.approve": "批准",
  "approval.approving": "批准中…",
  "approval.deny": "拒绝",
  "approval.denying": "拒绝中…",
  "approval.waitingEarlier": "等待上方的审批完成…",
  "clientTool.failedInBrowser": "{tool} 在浏览器中执行失败",
  "clientTool.runningInBrowser": "正在浏览器中运行 {tool}…",
  "clientTool.retry": "重试",
  "clientTool.retrying": "重试中…",
  "ask.submit": "提交",
  "ask.sending": "发送中…",
  "ask.skip": "跳过",
  "ask.customPlaceholder": "或输入你自己的回答…",
  "ask.customAria": "自定义回答",
  "profile.exportData": "导出我的数据",
  "login.phone": "手机号",
  "login.phoneSubtitle": "使用手机号 + 验证码登录或注册。",
  "login.sendCode": "发送验证码",
  "login.resendIn": "重新发送",
  "login.code": "验证码",
  "login.backToEmail": "返回邮箱登录",
  "login.totpHint": "此账号已开启两步验证,请输入身份验证器 App 中的动态验证码。",
  "login.totpCode": "动态验证码",
  "login.forgotPassword": "忘记密码?",
  "login.forgotHint": "请联系管理员重置密码,或使用手机号验证码登录。",
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
  "admin.settings": "运行设置",
  "admin.backToChat": "返回聊天",
  "lang.switchToZh": "切换为中文",
  "lang.switchToEn": "Switch to English",
  "settingsPage.title": "平台设置",
  "settingsPage.description": "运行时配置 — 更改立即生效,无需重启。保存空值会恢复环境默认值。",
  "settingsPage.group.tools": "工具",
  "settingsPage.group.webhooks": "Webhooks",
  "settingsPage.group.llm": "LLM / 模型",
  "settingsPage.group.sandbox": "沙箱",
  "settingsPage.group.permissions": "权限",
  "settingsPage.group.redaction": "脱敏",
  "settingsPage.group.subagents": "子代理",
  "settingsPage.group.background": "后台任务",
  "settingsPage.group.http": "HTTP / 网关",
  "settingsPage.group.auth": "认证 / SSO",
  "settingsPage.group.integrations": "集成",
  "settingsPage.clearOverride": "清除覆盖值",
  "settingsPage.permissionTitle": "执行权限策略",
  "settingsPage.permissionDescription": "为每个工具风险级别配置一个矩阵 — 点击判定立即生效。「ask」会挂起运行等待审批(无头运行视作拒绝)。",
  "settingsPage.currentValue": "当前值:",
  "settingsPage.save": "保存",
  "settingsPage.saving": "保存中…",
  "settingsPage.applied": "已生效 — 无需重启。",
  "settingsPage.nothingToChange": "没有需要更改的内容。",
  "settingsPage.loading": "正在加载设置",
  "settingsPage.notSet": "(未设置)",
  "settingsPage.secretSet": "(已设置 — 已隐藏)",
  "usersPage.title": "用户",
  "usersPage.description": "平台上的所有账号。禁用账号会立即吊销其令牌;删除账号会移除其会话与成员关系。",
  "usersPage.searchPlaceholder": "按邮箱或显示名称搜索",
  "usersPage.search": "搜索",
  "usersPage.loading": "正在加载账号",
  "usersPage.colAccount": "账号",
  "usersPage.colRole": "平台角色",
  "usersPage.colEnabled": "启用",
  "usersPage.colJoined": "加入时间",
  "usersPage.noAccounts": "没有匹配的账号。",
  "usersPage.range": "{from}–{to} / 共 {total}",
  "usersPage.previous": "上一页",
  "usersPage.next": "下一页",
  "usersPage.you": "你",
  "usersPage.roleUser": "用户",
  "usersPage.roleAdmin": "管理员",
  "usersPage.roleAria": "{email} 的平台角色",
  "usersPage.enableAria": "启用 {email}",
  "usersPage.deleteAccount": "删除账号",
  "usersPage.deleteTitle": "删除 {email}?",
  "usersPage.deleteDescription": "该账号及其会话、对话和成员关系将被永久删除。禁用则保留数据并阻止登录。",
  "usersPage.resetTitle": "重置密码",
  "usersPage.resetDescription": "为 {email} 设置新密码,并注销其所有设备。请通过可信渠道告知新密码。",
  "usersPage.newPassword": "新密码",
  "usersPage.minLength": "至少 8 个字符。",
  "usersPage.reset": "重置",
  "usersPage.newAccount": "新建账号",
  "usersPage.createTitle": "创建账号",
  "usersPage.createDescription": "没有邀请邮件 — 请自行设置初始密码并转告对方。持有者可在个人资料中修改密码。",
  "usersPage.email": "邮箱",
  "usersPage.displayName": "显示名称",
  "usersPage.namePlaceholder": "默认为邮箱地址",
  "usersPage.initialPassword": "初始密码",
  "usersPage.creating": "创建中…",
  "usersPage.create": "创建",
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
  "chat.emptyState": "Ask anything, or have me work with files in your workspace.",
  "chat.rerunTitle": "Re-run the previous message",
  "chat.retry": "Retry",
  "chat.disclaimer": "nowhere-agent can read and write files in your workspace. Double-check important output.",
  "chat.searchChats": "Search chats",
  "chat.loadError": "Couldn’t load conversations.",
  "chat.noConversations": "No conversations yet",
  "chat.noConversationsHint": "Start one with “New chat”.",
  "chat.noMatches": "No matches",
  "chat.noMatchesHint": "Nothing matches “{term}”.",
  "chat.deleteConversation": "Delete conversation",
  "chat.deleteConfirm": "Delete this conversation? This can’t be undone.",
  "chat.deleteFailed": "Couldn’t delete the conversation — try again.",
  "chat.untitled": "Untitled",
  "chat.searching": "Searching…",
  "chat.loadingMore": "Loading…",
  "chat.webpImage": "WebP image",
  "approval.approveRunning": "Approve running {tool}?",
  "approval.approve": "Approve",
  "approval.approving": "Approving…",
  "approval.deny": "Deny",
  "approval.denying": "Denying…",
  "approval.waitingEarlier": "Waiting for the earlier approval above…",
  "clientTool.failedInBrowser": "{tool} failed in your browser",
  "clientTool.runningInBrowser": "Running {tool} in your browser…",
  "clientTool.retry": "Retry",
  "clientTool.retrying": "Retrying…",
  "ask.submit": "Submit",
  "ask.sending": "Sending…",
  "ask.skip": "Skip",
  "ask.customPlaceholder": "Or type your own answer…",
  "ask.customAria": "Custom answer",
  "profile.exportData": "Export my data",
  "login.phone": "Phone",
  "login.phoneSubtitle": "Sign in or register with a mobile number and one-time code.",
  "login.sendCode": "Send code",
  "login.resendIn": "Resend in",
  "login.code": "Verification code",
  "login.backToEmail": "Back to email sign-in",
  "login.totpHint": "This account has two-step verification enabled. Enter the code from your authenticator app.",
  "login.totpCode": "Authenticator code",
  "login.forgotPassword": "Forgot password?",
  "login.forgotHint": "Contact your administrator to reset it, or sign in with a phone number and one-time code.",
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
  "admin.settings": "Settings",
  "admin.backToChat": "Back to chat",
  "lang.switchToZh": "切换到中文",
  "lang.switchToEn": "Switch to English",
  "settingsPage.title": "Platform settings",
  "settingsPage.description": "Runtime configuration — changes apply immediately, no restart. Saving an empty value restores the environment default.",
  "settingsPage.group.tools": "Tools",
  "settingsPage.group.webhooks": "Webhooks",
  "settingsPage.group.llm": "LLM / model",
  "settingsPage.group.sandbox": "Sandbox",
  "settingsPage.group.permissions": "Permissions",
  "settingsPage.group.redaction": "Redaction",
  "settingsPage.group.subagents": "Subagents",
  "settingsPage.group.background": "Background tasks",
  "settingsPage.group.http": "HTTP / gateway",
  "settingsPage.group.auth": "Auth / SSO",
  "settingsPage.group.integrations": "Integrations",
  "settingsPage.clearOverride": "Clear override",
  "settingsPage.permissionTitle": "Execution-permission policy",
  "settingsPage.permissionDescription": "One matrix for every tool risk class — click a verdict to apply it immediately. \"ask\" suspends the run for approval (headless runs treat it as deny).",
  "settingsPage.currentValue": "Current value:",
  "settingsPage.save": "Save",
  "settingsPage.saving": "Saving…",
  "settingsPage.applied": "Applied — no restart needed.",
  "settingsPage.nothingToChange": "Nothing to change.",
  "settingsPage.loading": "Loading settings",
  "settingsPage.notSet": "(not set)",
  "settingsPage.secretSet": "(set — hidden)",
  "usersPage.title": "Users",
  "usersPage.description": "Every account on the platform. Disabling an account revokes its tokens immediately; deleting one removes its sessions and memberships.",
  "usersPage.searchPlaceholder": "Search by email or display name",
  "usersPage.search": "Search",
  "usersPage.loading": "Loading accounts",
  "usersPage.colAccount": "Account",
  "usersPage.colRole": "Platform role",
  "usersPage.colEnabled": "Enabled",
  "usersPage.colJoined": "Joined",
  "usersPage.noAccounts": "No accounts match.",
  "usersPage.range": "{from}–{to} of {total}",
  "usersPage.previous": "Previous",
  "usersPage.next": "Next",
  "usersPage.you": "You",
  "usersPage.roleUser": "User",
  "usersPage.roleAdmin": "Administrator",
  "usersPage.roleAria": "Platform role for {email}",
  "usersPage.enableAria": "Enable {email}",
  "usersPage.deleteAccount": "Delete account",
  "usersPage.deleteTitle": "Delete {email}?",
  "usersPage.deleteDescription": "The account, its sessions, conversations, and memberships are removed permanently. Disabling instead keeps the data and blocks sign-in.",
  "usersPage.resetTitle": "Reset password",
  "usersPage.resetDescription": "Sets a new password for {email} and signs out every device they have. Tell them the new password over a channel you trust.",
  "usersPage.newPassword": "New password",
  "usersPage.minLength": "At least 8 characters.",
  "usersPage.reset": "Reset",
  "usersPage.newAccount": "New account",
  "usersPage.createTitle": "Create an account",
  "usersPage.createDescription": "There is no invitation email — set an initial password and pass it on yourself. The holder can change it from their profile.",
  "usersPage.email": "Email",
  "usersPage.displayName": "Display name",
  "usersPage.namePlaceholder": "Defaults to the email address",
  "usersPage.initialPassword": "Initial password",
  "usersPage.creating": "Creating…",
  "usersPage.create": "Create",
};

// lang resolves the UI language once: a stored choice (nowhere.lang) wins,
// else the browser locale (zh-* → zh). setLang persists the choice and
// notifies subscribers; t() reads the mutable value at call time, so text
// renders in the new language on the next render of its component.
const LANG_KEY = "nowhere.lang";
function detectLang(): "zh" | "en" {
  try {
    const stored = localStorage.getItem(LANG_KEY);
    if (stored === "zh" || stored === "en") return stored;
  } catch {
    // localStorage unavailable (private mode); fall through to the locale.
  }
  return navigator.language.toLowerCase().startsWith("zh") ? "zh" : "en";
}
let lang: "zh" | "en" = detectLang();
const langListeners = new Set<() => void>();

// getLang returns the active UI language.
export function getLang(): "zh" | "en" {
  return lang;
}

// setLang switches the UI language and persists the choice; subscribers
// (useLang) re-render. Components rendering via plain t() pick the new
// language up on their next render.
export function setLang(next: "zh" | "en") {
  if (next === lang) return;
  lang = next;
  try {
    localStorage.setItem(LANG_KEY, next);
  } catch {
    // best-effort persistence; the switch still applies for this session
  }
  for (const fn of langListeners) fn();
}

function subscribeLang(fn: () => void) {
  langListeners.add(fn);
  return () => langListeners.delete(fn);
}

// useLang reactively returns the active UI language, re-rendering the caller
// when setLang switches it.
export function useLang(): "zh" | "en" {
  return useSyncExternalStore(subscribeLang, getLang);
}

// t returns the localized string for key. vars, when given, substitutes
// {name} placeholders in the translation (e.g. t("chat.noMatchesHint", {term})).
export function t(key: I18nKey, vars?: Record<string, string | number>): string {
  const dict = lang === "zh" ? zh : en;
  let s = dict[key];
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      s = s.replaceAll(`{${k}}`, String(v));
    }
  }
  return s;
}

// isZh reports whether the UI is currently rendering Chinese, for the rare
// components that need to branch beyond plain strings.
export function isZh(): boolean {
  return lang === "zh";
}

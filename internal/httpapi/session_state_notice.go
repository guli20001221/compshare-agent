package httpapi

// Context-degradation notices.
//
// When a turn cannot read the session's persisted state, the user MUST be told.
// The alternative — what this code did before — is to log a warning nobody
// reads, wipe the state, and answer anyway. From the user's side that is
// indistinguishable from the agent quietly forgetting which instance they were
// talking about, and it is the exact failure the session-state work exists to
// remove.
//
// These strings are appended by the handler, deterministically. They are NOT
// put in the prompt. A warning routed through the model is a warning the model
// may paraphrase, soften, or drop entirely — it has done all three (see
// internal/renderer: the grounded renderer rewrites anything that is not
// carried verbatim in the envelope). "Tell the user" is a guarantee, so the
// code that guarantees it must not be an LLM.
//
// The two cases are deliberately NOT handled the same way. See prepareChat.
const (
	// noticeSessionStateReset is shown when sessions.context could not be
	// parsed at all — the row is broken JSON. Nothing is recoverable, so the
	// state is reset to empty AND persisted: the session heals on this turn
	// instead of re-failing on every turn forever.
	noticeSessionStateReset = "⚠️ 我没能读取这个会话之前的上下文（记录已损坏），本轮从空白状态开始，之后会恢复正常。如果你之前指定过实例或有正在进行的操作，请再说一遍。\n\n"

	// noticeSessionStateUnreadable is shown when sessions.context carries an
	// agent envelope whose schema_version this binary does not know — it was
	// written by a NEWER binary and we have been rolled back.
	//
	// The data is INTACT; this binary simply cannot read it. So this turn
	// degrades (answers without the state) and — critically — does NOT persist.
	// Writing here would overwrite a newer binary's state with an older
	// binary's understanding of it, turning a reversible rollback into
	// permanent, session-by-session data loss across every session the rolled-
	// back binary happens to touch.
	//
	// Refusing the turn outright is not an option either: the row stays
	// unreadable, so "please retry" is a lie and the session is bricked for as
	// long as the rollback lasts. Degrade, disclose, do not write.
	noticeSessionStateUnreadable = "⚠️ 这个会话的上下文由更新版本的服务写入，当前版本读不了，本轮不带历史状态作答（不会覆盖它）。如果你之前指定过实例，请再说一遍。\n\n"
)

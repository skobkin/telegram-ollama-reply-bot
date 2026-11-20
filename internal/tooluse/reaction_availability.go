package tooluse

import (
	"context"
	"slices"

	"telegram-ollama-reply-bot/internal/telegram/reactionavailability"
	"telegram-ollama-reply-bot/internal/telegram/reactionemoji"
)

type reactionAvailability interface {
	Lookup(context.Context, int64, string) reactionavailability.Snapshot
	Peek(int64) reactionavailability.Snapshot
	Invalidate(int64)
}

func (r *Runtime) AttachReactionAvailability(provider *reactionavailability.Provider) {
	if provider != nil {
		r.reactionAvailability = provider
	}
}

func (r *Runtime) reactionTargetSuitable(call CallContext) bool {
	target, ok := setReactionTarget(call)

	return ok && target.chatID != 0 && target.messageID > 0 && !call.Requester.IsMe && call.RequesterMediaGroupID == "" && !r.config.BotIdentity.zero() && r.actions != nil
}

func (r *Runtime) prepareReactionAvailability(ctx context.Context, req ChatRequest, run *runState) {
	if req.AllowedTools != nil && !slices.Contains(req.AllowedTools, "set_reaction") {
		return
	}
	if !run.canReact() || !r.reactionTargetSuitable(r.callContext(req, run)) {
		return
	}
	run.reactionCandidates = reactionemoji.ReactionCandidates()
	if r.reactionAvailability != nil {
		run.reactionCandidates = r.reactionAvailability.Lookup(ctx, req.Scope.ChatID, req.ChatTitle).Candidates
	}
}

func (r *Runtime) reactionPresentation(call CallContext, p Presentation) (Presentation, bool) {
	if !r.reactionTargetSuitable(call) || call.run == nil || !call.run.canReact() {
		return p, false
	}
	candidates := r.currentReactionCandidates(call)
	if len(candidates) == 0 {
		return p, false
	}
	p.Parameters = reactionParameters(candidates)

	return p, true
}

// Preserve the advertised run snapshot while respecting newer local cache
// restrictions. Cache eviction must not broaden a restricted candidate set.
func (r *Runtime) currentReactionCandidates(call CallContext) []string {
	candidates := reactionemoji.ReactionCandidates()
	if call.run != nil && call.run.reactionCandidates != nil {
		candidates = slices.Clone(call.run.reactionCandidates)
	}
	if r.reactionAvailability != nil {
		current := r.reactionAvailability.Peek(call.Scope.ChatID)
		if current.Known {
			candidates = slices.DeleteFunc(candidates, func(emoji string) bool { return !slices.Contains(current.Candidates, emoji) })
		}
	}
	if call.run != nil {
		candidates = slices.DeleteFunc(candidates, func(emoji string) bool { return call.run.reactionRejected[emoji] })
	}

	return candidates
}

func (run *runState) canReact() bool {
	return run != nil && !run.reactionSucceeded && run.reactionAttempts < 3 && run.reactionBlocked == ""
}

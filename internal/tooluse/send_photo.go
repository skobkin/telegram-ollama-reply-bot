package tooluse

import (
	"context"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// sendPhotoToScope uploads data as a photo into the call's chat (and topic,
// when the scope has one) through the runtime's action sender, which is the
// event-publishing wrapper in production — so MessageSent consumers see the
// photo like any other bot send. caption is sent as plain text (no parse
// mode) and may be empty.
//
// It does not check ctx before dispatch: callers decide how an expired turn
// is reported (the tools return a not-applied "cancelled" result before
// calling this), and an error here is raw so ClassifyError marks the outcome
// ambiguous — the upload may have reached Telegram.
func (r *Runtime) sendPhotoToScope(ctx context.Context, callCtx CallContext, data []byte, filename, caption string) (*t.Message, error) {
	params := tu.Photo(tu.ID(callCtx.Scope.ChatID), tu.FileFromBytes(data, filename))
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}
	if caption != "" {
		params.Caption = caption
	}

	return r.actions.SendPhoto(ctx, params)
}

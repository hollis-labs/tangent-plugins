package torqueclient

import "context"

// ParentGuard must resolve current ordinary parent authority. It returns the
// actual verified principal, not a principal copied from body/filter labels.
// It must honor context. It cannot create child grants or credential epochs.
type ParentGuard func(context.Context) (string, error)

// Bound is an immutable request-local view. Only capacity and partitioned
// metadata storage are shared; proof and parent guard never mutate the client.
type Bound struct {
	client *Client
	proof  any
	parent ParentGuard
}

func (c *Client) Bind(proof any, parent ParentGuard) *Bound {
	return &Bound{client: c, proof: proof, parent: parent}
}

func (b *Bound) Read(ctx context.Context, name string, input map[string]any) (any, error) {
	if b == nil || b.parent == nil {
		return nil, Denied
	}
	return b.client.read(ctx, b.proof, name, input, b.parent)
}

func (b *Bound) LatestCommentActivity(ctx context.Context, ids []string) (Activity, error) {
	if b == nil || b.parent == nil {
		return Activity{}, Denied
	}
	return b.client.activity(ctx, b.proof, ids, b.parent)
}

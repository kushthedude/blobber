package node

import (
	"context"

	"github.com/0chain/blobber/code/go/0chain.net/core/common"
)

const SELF_NODE common.ContextKey = "SELF_NODE"

/*GetNodeContext - setup a context with the self node */
func GetNodeContext() context.Context {
	return context.WithValue(context.Background(), SELF_NODE, Self)
}

/*GetSelfNode - given a context, return the self node associated with it */
func GetSelfNode(ctx context.Context) *SelfNode {
	return ctx.Value(SELF_NODE).(*SelfNode)
}

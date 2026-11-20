// ctx_test.go holds the one tiny helper the reload tests share: a context that ends when a channel closes.
package machines

import "context"

func ctxDone(stop <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-stop; cancel() }()
	return ctx
}

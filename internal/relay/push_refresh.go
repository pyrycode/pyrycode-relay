package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// fcmTokenCache shares an attempt's failure as well as its token. The JWT
// source's reuse mutex alone would make each failed waiter retry in turn.
type fcmTokenCache struct {
	cfg    *jwt.Config
	client *http.Client

	mu      sync.Mutex
	cached  *oauth2.Token
	refresh *fcmTokenRefresh
}

type fcmTokenRefresh struct {
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int // guarded by fcmTokenCache.mu

	// Published before done closes; immutable afterwards.
	token *oauth2.Token
	err   error
}

// RefreshState snapshots the FCM refresh's joined waiters and completion
// channel for the process shutdown regression. It exposes no credentials and
// performs no I/O or mutation. Retain done to observe that attempt's cleanup
// even after the cache removes it. Non-FCM senders have no refresh to observe.
func (w *PushWaker) RefreshState() (waiters int, done <-chan struct{}) {
	if w == nil {
		return 0, nil
	}
	s, ok := w.sender.(*FCMSender)
	if !ok {
		return 0, nil
	}
	s.tokens.mu.Lock()
	defer s.tokens.mu.Unlock()
	if r := s.tokens.refresh; r != nil {
		return r.waiters, r.done
	}
	return 0, nil
}

func (c *fcmTokenCache) token(ctx context.Context) (*oauth2.Token, error) {
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", ErrFCMTokenFetch, err)
	}
	if c.cached.Valid() {
		tok := c.cached
		c.mu.Unlock()
		return tok, nil
	}
	r := c.refresh
	if r == nil {
		// No single caller owns cancellation: even the starter can leave
		// while another waiter still needs the authentication request.
		refreshCtx, cancel := context.WithTimeout(context.Background(), fcmSendTimeout)
		r = &fcmTokenRefresh{cancel: cancel, done: make(chan struct{})}
		c.refresh = r
		go c.fetch(refreshCtx, r)
	}
	r.waiters++
	c.mu.Unlock()
	defer c.leave(r)

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrFCMTokenFetch, ctx.Err())
	case <-r.done:
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFCMTokenFetch, err)
		}
		return r.token, r.err
	}
}

func (c *fcmTokenCache) leave(r *fcmTokenRefresh) {
	c.mu.Lock()
	r.waiters--
	last := r.waiters == 0 && c.refresh == r
	if last {
		c.refresh = nil
		r.cancel()
	}
	c.mu.Unlock()
	if last {
		// Keep refresh cleanup inside the last Send's lifetime so the
		// waker's send wait group also joins authentication on shutdown.
		<-r.done
	}
}

func (c *fcmTokenCache) fetch(ctx context.Context, r *fcmTokenRefresh) {
	client := *c.client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = fcmRefreshTransport{ctx: ctx, base: base}
	tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, &client)
	tok, err := c.cfg.TokenSource(tokenCtx).Token()
	if err != nil {
		// RetrieveError embeds the endpoint body, and other JWT errors
		// may contain a credential-bearing URL. Publish only the status.
		var reply *oauth2.RetrieveError
		if errors.As(err, &reply) && reply.Response != nil {
			err = fmt.Errorf("%w: status %d", ErrFCMTokenFetch, reply.Response.StatusCode)
		} else {
			err = ErrFCMTokenFetch
		}
	}
	r.cancel()

	c.mu.Lock()
	if c.refresh == r {
		c.refresh = nil
		if err == nil {
			c.cached = tok
		}
	}
	r.token, r.err = tok, err
	close(r.done)
	c.mu.Unlock()
}

// jwtSource.Token uses http.Client.PostForm, which does not attach the source
// context to its request. Bind it here so cancellation reaches the network.
type fcmRefreshTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t fcmRefreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.Clone(t.ctx))
}

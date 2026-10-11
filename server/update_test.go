package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer that protects the GitHub quota: unauthenticated API only has 60 requests/hour/IP,
// and the top-bar "has new version" tooltip queries on every full page load. Once the cache expires, a user opening several
// tabs burns through the quota, and when they actually want to update the query fails instead.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should only hit the source once, got %d times", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// User clicking "check for updates" must get a real-time result, otherwise a just-published version has to wait for the cache to expire to be visible.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass cache, expected 2 source hits, got %d times", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Move the cached time forward to just past expiry, simulating TTL elapsing.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("should re-fetch after TTL expires, expected 2 source hits, got %d times", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	// Failed results should also be cached briefly, otherwise when GitHub is unreachable every page load waits for a timeout.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 1 {
		t.Errorf("error should be cached briefly, expected 1 source hit, got %d times", calls)
	}

	// But the error TTL must be clearly shorter than the success TTL, so the cache self-heals quickly once the network is restored.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL(%v) must be shorter than success TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 2 {
		t.Errorf("after error TTL expires should retry, expected 2 times, got %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// Closing a tab will cancel the request. That doesn't mean GitHub has a problem — we must not write
	// "cancelled" into the cache, otherwise every visitor for the next 30 minutes would see a strange error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // let the cache expire to force a re-fetch

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("when the caller cancels, the error should be passed through")
	}

	// Key invariant: the cancelled call leaves no trace — the cache has no "cancelled" error
	// and still holds the previous good result.
	if c.err != nil {
		t.Fatalf("cancelled error should not be written to the cache, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("cache should retain the previous good result, got %+v", c.rel)
	}

	// That cancelled call didn't fetch any new data, so the next visitor should re-fetch — and get a normal
	// result, not be tainted by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("normal request after cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("should get a normal result, got %+v", rel)
	}
}

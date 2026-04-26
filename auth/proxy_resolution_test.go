package auth

import "testing"

func TestResolveProxyForAccountPrefersAccountProxy(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
	}
	account := &Account{
		DBID:     7,
		ProxyURL: " http://account-proxy:8080 ",
	}

	got := store.ResolveProxyForAccount(account)
	want := "http://account-proxy:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

func TestResolveProxyForAccountUsesStickyProxyPool(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool: []string{
			"http://pool-1:8080",
			"http://pool-2:8080",
			"http://pool-3:8080",
		},
	}

	cases := []struct {
		id   int64
		want string
	}{
		{id: 1, want: "http://pool-1:8080"},
		{id: 2, want: "http://pool-2:8080"},
		{id: 3, want: "http://pool-3:8080"},
		{id: 4, want: "http://pool-1:8080"},
	}
	for _, tc := range cases {
		account := &Account{DBID: tc.id}
		first := store.ResolveProxyForAccount(account)
		second := store.ResolveProxyForAccount(account)
		if first != tc.want || second != tc.want {
			t.Fatalf("account %d proxy = %q/%q, want sticky %q", tc.id, first, second, tc.want)
		}
	}
}

func TestResolveProxyForAccountFallsBackToGlobalProxy(t *testing.T) {
	store := &Store{
		globalProxy:      " http://global-proxy:8080 ",
		proxyPoolEnabled: false,
		proxyPool:        []string{"http://pool-1:8080"},
	}

	got := store.ResolveProxyForAccount(&Account{DBID: 1})
	want := "http://global-proxy:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

func TestResolveProxyForAccountSkipsBlankPoolEntries(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"", " http://pool-2:8080 "},
	}

	got := store.ResolveProxyForAccount(&Account{DBID: 1})
	want := "http://pool-2:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

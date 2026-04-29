package proxy

import (
	"fmt"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/webimage"
	"github.com/google/uuid"
)

func stableWebImageIdentity(kind string, account *auth.Account) string {
	if account == nil {
		return uuid.NewString()
	}
	accountID := ""
	email := ""
	account.Mu().RLock()
	accountID = strings.TrimSpace(account.AccountID)
	email = strings.ToLower(strings.TrimSpace(account.Email))
	account.Mu().RUnlock()
	seed := fmt.Sprintf("codex2api:webimage:%s:%d:%s:%s", kind, account.ID(), accountID, email)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func NewWebImageClient(account *auth.Account, proxyURL string) (*webimage.Client, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := ""
	account.Mu().RLock()
	accessToken = strings.TrimSpace(account.AccessToken)
	account.Mu().RUnlock()
	if accessToken == "" {
		return nil, fmt.Errorf("account %d missing access token", account.ID())
	}
	return webimage.New(webimage.Options{
		AuthToken: accessToken,
		DeviceID:  stableWebImageIdentity("device", account),
		SessionID: stableWebImageIdentity("session", account),
		ProxyURL:  strings.TrimSpace(proxyURL),
	})
}

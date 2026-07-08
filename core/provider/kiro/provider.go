// Package kiro speaks AWS CodeWhisperer: it builds a CodeWhisperer body from the
// normalized request, signs it with a refreshing token, and decodes the binary
// event-stream reply back into normalized events.
package kiro

import (
	"bytes"
	"net/http"
	"sync"

	"github.com/enowdev/enowx/core/model"
	"github.com/enowdev/enowx/core/provider"
	"github.com/enowdev/enowx/core/transport"
)

const endpoint = "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse"

// CredSaver persists refreshed credentials for an account.
type CredSaver func(id int64, creds map[string]string)

type Provider struct {
	doer transport.Doer
	save CredSaver

	mu       sync.Mutex
	managers map[int64]*authManager
}

func New(doer transport.Doer, save CredSaver) *Provider {
	return &Provider{doer: doer, save: save, managers: map[int64]*authManager{}}
}

func (p *Provider) Name() string        { return "kiro" }
func (p *Provider) Caps() provider.Caps { return provider.Caps{Chat: true} }

func (p *Provider) manager(acc provider.Account) *authManager {
	p.mu.Lock()
	defer p.mu.Unlock()
	if am, ok := p.managers[acc.ID]; ok {
		return am
	}
	am := newAuthManager(p.doer, p.save, acc)
	p.managers[acc.ID] = am
	return am
}

func (p *Provider) BuildRequest(req *model.Request, acc provider.Account) (*http.Request, error) {
	am := p.manager(acc)
	token, err := am.token()
	if err != nil {
		return nil, err
	}
	body, reverse, err := buildPayload(req, am.profileARN(), "")
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range am.headers(token) {
		r.Header.Set(k, v)
	}
	if len(reverse) > 0 {
		r = r.WithContext(withReverseNames(r.Context(), reverse))
	}
	return r, nil
}

func (p *Provider) ParseResponse(resp *http.Response, _ *model.Request) (model.Stream, error) {
	return newStream(resp, reverseNamesFrom(resp.Request.Context())), nil
}

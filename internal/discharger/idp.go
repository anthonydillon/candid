// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package discharger

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"golang.org/x/net/trace"
	candidclient "gopkg.in/CanonicalLtd/candidclient.v1"
	"gopkg.in/CanonicalLtd/candidclient.v1/params"
	"gopkg.in/errgo.v1"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
	macaroon "gopkg.in/macaroon.v2"

	"github.com/CanonicalLtd/candid/idp"
	"github.com/CanonicalLtd/candid/idp/idputil/secret"
	"github.com/CanonicalLtd/candid/internal/auth"
	"github.com/CanonicalLtd/candid/internal/discharger/internal"
	"github.com/CanonicalLtd/candid/internal/identity"
	"github.com/CanonicalLtd/candid/store"
)

const (
	// identityMacaroonDuration is the length of time for which an
	// identity macaroon is valid.
	identityMacaroonDuration = 28 * 24 * time.Hour

	// dischargeTokenDuration is the length of time for which a
	// discharge token is valid.
	dischargeTokenDuration = identityMacaroonDuration
)

type initIDPParams struct {
	identity.HandlerParams
	Codec                 *secret.Codec
	DischargeTokenCreator *dischargeTokenCreator
	VisitCompleter        *visitCompleter
}

func initIDPs(ctx context.Context, params initIDPParams) error {
	for _, ip := range params.IdentityProviders {
		kvStore, err := params.ProviderDataStore.KeyValueStore(ctx, ip.Name())
		if err != nil {
			return errgo.Mask(err)
		}
		if err := ip.Init(ctx, idp.InitParams{
			Store:                 params.Store,
			KeyValueStore:         kvStore,
			Oven:                  params.Oven,
			Codec:                 params.Codec,
			Location:              params.Location,
			URLPrefix:             params.Location + "/login/" + ip.Name(),
			DischargeTokenCreator: params.DischargeTokenCreator,
			VisitCompleter:        params.VisitCompleter,
			Template:              params.Template,
		}); err != nil {
			return errgo.Mask(err)
		}
	}
	return nil
}

func newIDPHandler(params identity.HandlerParams, idp idp.IdentityProvider) httprouter.Handle {
	return func(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
		t := trace.New("identity.internal.v1.idp", idp.Name())
		defer t.Finish()
		ctx := trace.NewContext(context.Background(), t)
		ctx, close := params.Store.Context(ctx)
		defer close()
		ctx, close = params.MeetingStore.Context(ctx)
		defer close()
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/login/"+idp.Name())
		req.ParseForm()
		idp.Handle(ctx, w, req)
	}
}

type dischargeTokenCreator struct {
	params identity.HandlerParams
}

func (d *dischargeTokenCreator) DischargeToken(ctx context.Context, id *store.Identity) (*httpbakery.DischargeToken, error) {
	m, err := d.params.Oven.NewMacaroon(
		ctx,
		bakery.LatestVersion,
		[]checkers.Caveat{
			checkers.TimeBeforeCaveat(time.Now().Add(dischargeTokenDuration)),
			candidclient.UserDeclaration(id.Username),
		},
		identchecker.LoginOp,
	)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	v, err := m.M().MarshalBinary()
	if err != nil {
		return nil, errgo.Mask(err)
	}
	id.LastLogin = time.Now()
	if err := d.params.Store.UpdateIdentity(ctx, id, store.Update{
		store.LastLogin: store.Set,
	}); err != nil {
		logger.Errorf("cannot update last login time: %s", err)
	}
	return &httpbakery.DischargeToken{
		Kind:  "macaroon",
		Value: v,
	}, nil
}

// A visitCompleter is an implementation of idp.VisitCompleter.
type visitCompleter struct {
	params                identity.HandlerParams
	dischargeTokenCreator *dischargeTokenCreator
	dischargeTokenStore   *internal.DischargeTokenStore
	place                 *place
}

// Success implements idp.VisitCompleter.Success.
func (c *visitCompleter) Success(ctx context.Context, w http.ResponseWriter, req *http.Request, dischargeID string, id *store.Identity) {
	dt, err := c.dischargeTokenCreator.DischargeToken(ctx, id)
	if err != nil {
		c.Failure(ctx, w, req, dischargeID, errgo.Mask(err))
		return
	}
	c.successToken(ctx, w, req, dischargeID, dt, id)
}

func (c *visitCompleter) successToken(ctx context.Context, w http.ResponseWriter, req *http.Request, dischargeID string, dt *httpbakery.DischargeToken, id *store.Identity) {
	if dischargeID != "" {
		if err := c.place.Done(ctx, dischargeID, &loginInfo{DischargeToken: dt}); err != nil {
			c.Failure(ctx, w, req, dischargeID, errgo.Mask(err))
			return
		}
	}
	if id == nil {
		id = &store.Identity{
			Username: usernameFromDischargeToken(dt),
		}
		if err := c.params.Store.Identity(ctx, id); err != nil {
			// Log, but otherwise ignore this error, the username is probably enough.
			logger.Errorf("cannot look up user identity: %s", err)
		}
	}
	t := c.params.Template.Lookup("login")
	if t == nil {
		fmt.Fprintf(w, "Login successful as %s", id.Username)
		return
	}
	w.Header().Set("Content-Type", "text/html;charset=utf-8")
	if err := t.Execute(w, id); err != nil {
		logger.Errorf("error processing login template: %s", err)
	}
}

// Failure implements idp.VisitCompleter.Failure.
func (c *visitCompleter) Failure(ctx context.Context, w http.ResponseWriter, req *http.Request, dischargeID string, err error) {
	_, bakeryErr := httpbakery.ErrorToResponse(ctx, err)
	if dischargeID != "" {
		c.place.Done(ctx, dischargeID, &loginInfo{
			Error: bakeryErr.(*httpbakery.Error),
		})
	}
	identity.WriteError(ctx, w, err)
}

// RedirectSuccess implements idp.VisitCompleter.RedirectSuccess.
func (c *visitCompleter) RedirectSuccess(ctx context.Context, w http.ResponseWriter, req *http.Request, returnTo, state string, id *store.Identity) {
	dt, err := c.dischargeTokenCreator.DischargeToken(ctx, id)
	if err != nil {
		c.RedirectFailure(ctx, w, req, returnTo, state, errgo.Mask(err))
		return
	}
	code, err := c.dischargeTokenStore.Put(ctx, dt, time.Now().Add(10*time.Minute))
	if err != nil {
		c.RedirectFailure(ctx, w, req, returnTo, state, errgo.Mask(err))
		return
	}
	v := url.Values{
		"code": {code},
	}
	if state != "" {
		v.Set("state", state)
	}
	if err := c.redirect(w, req, returnTo, v); err != nil {
		identity.WriteError(ctx, w, err)
	}
	return
}

// RedirectFailure implements idp.VisitCompleter.RedirectFailure.
func (c *visitCompleter) RedirectFailure(ctx context.Context, w http.ResponseWriter, req *http.Request, returnTo, state string, err error) {
	v := url.Values{
		"error": {err.Error()},
	}
	if state != "" {
		v.Set("state", state)
	}
	if ec, ok := errgo.Cause(err).(params.ErrorCode); ok {
		v.Set("error_code", string(ec))
	}
	if rerr := c.redirect(w, req, returnTo, v); rerr == nil {
		return
	}
	identity.WriteError(ctx, w, err)
}

// redirect writes a redirect response addressed the the given returnTo
// address with the given query parameters. If an error is returned it
// will be because the returnTo address is invalid and therefore it will
// not be possible to redirect to it.
func (c *visitCompleter) redirect(w http.ResponseWriter, req *http.Request, returnTo string, query url.Values) error {
	u, err := url.Parse(returnTo)
	// We only support logins from ourselves for now, so only allow a
	// redirect if it returns back to ourselves.
	if !strings.HasPrefix(returnTo, c.params.Location) || err != nil {
		return errgo.WithCausef(err, params.ErrBadRequest, "invalid return_to")
	}
	q := u.Query()
	for k, v := range query {
		q[k] = append(q[k], v...)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, req, u.String(), http.StatusSeeOther)
	return nil
}

func usernameFromDischargeToken(dt *httpbakery.DischargeToken) string {
	if dt.Kind != "macaroon" {
		return ""
	}
	var m macaroon.Macaroon
	if err := m.UnmarshalBinary(dt.Value); err != nil {
		return ""
	}
	return checkers.InferDeclared(auth.Namespace, macaroon.Slice{&m})["username"]
}

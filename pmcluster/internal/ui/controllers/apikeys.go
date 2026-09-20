package controllers

import (
	"context"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// APIKeys manages the bearer tokens the console uses to talk to a pmcluster
// daemon (the daemon minted its own on first run; these are the ones an operator
// rolls for CI or a second console).
//
// The token is shown exactly once, by Add: the daemon stores only a hash, so
// nothing can bring the plaintext back later. That is why the table shows a
// prefix-or-nothing column instead of a value, and why the one-time panel is the
// only place the full string ever appears.
type APIKeys struct{ *Controller }

// apiKeyRow is one key as the table shows it. Prefix is only ever filled for the
// key created in this response; a later page load cannot know it, and the
// template says so instead of printing a plausible-looking stub.
type apiKeyRow struct {
	ID          int64
	Name        string
	CreatedAt   int64
	Prefix      string
	JustCreated bool
}

type apiKeysData struct {
	Keys       []apiKeyRow
	Count      int
	Configured bool // a daemon is configured; false is its own page state
	Known      bool // the list call answered; false with ErrKey set is a failure
	ErrKey     string
	ErrRaw     string
	MsgKey     string
	MsgArg     string

	// One-time token panel: filled only by Add.
	Token       string
	TokenName   string
	TokenPrefix string
	NewID       int64
}

// fail records a failed list/create/revoke: key is the dictionary entry the
// fragment translates and raw is the daemon's raw reply for the disclosure.
func (d *apiKeysData) fail(key, raw string) { d.ErrKey, d.ErrRaw = key, raw }

// Page lists the keys. A failure to reach the daemon sets ErrKey (never an empty
// table — "we could not ask" and "there are none" are different answers).
func (c APIKeys) Page(g *gin.Context) {
	ctx := g.Request.Context()
	d := apiKeysData{}
	_, _, configured := c.loadParams(ctx)
	if !configured {
		// Not configured is its own state: there is no daemon to list keys from.
		c.Views.Fragment(g, "apikeys", d)
		return
	}
	d.Configured = true
	c.loadKeys(ctx, &d)
	c.Views.Fragment(g, "apikeys", d)
}

// Add creates a key and reveals its token once. The token is returned by the
// daemon in the create response and nowhere else, so it is handed to the
// fragment for this response only.
func (c APIKeys) Add(g *gin.Context) {
	ctx := g.Request.Context()
	name := strings.TrimSpace(g.PostForm("name"))
	d := apiKeysData{}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "apikeys", d)
		return
	}
	d.Configured = true
	if name == "" {
		d.fail("apikeys.err_name", "")
		c.loadKeys(ctx, &d)
		c.Views.Fragment(g, "apikeys", d)
		return
	}
	created, err := c.API.CreateAPIKey(ctx, name)
	if err != nil {
		d.fail("apikeys.err_create", err.Error())
	} else {
		d.Token, d.NewID = created.Token, created.ID
		d.TokenName, d.TokenPrefix = created.Name, tokenPrefix(created.Token)
		d.MsgKey, d.MsgArg = "apikeys.msg_created", created.Name
	}
	c.loadKeys(ctx, &d)
	c.markCreated(&d)
	c.Views.Fragment(g, "apikeys", d)
}

// Remove revokes a key by id. The row's button carries the confirmation; the
// controller still refuses a non-numeric or unknown id rather than calling the
// daemon with a guess.
func (c APIKeys) Remove(g *gin.Context) {
	ctx := g.Request.Context()
	d := apiKeysData{}
	if c.apiRefused(g, &d) {
		c.Views.Fragment(g, "apikeys", d)
		return
	}
	d.Configured = true
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		d.fail("apikeys.err_bad_id", "")
	} else if err := c.API.DeleteAPIKey(ctx, id); err != nil {
		d.fail("apikeys.err_revoke", err.Error())
	} else {
		d.MsgKey, d.MsgArg = "apikeys.msg_removed", strconv.FormatInt(id, 10)
	}
	c.loadKeys(ctx, &d)
	c.Views.Fragment(g, "apikeys", d)
}

// loadKeys fills the table. On failure Known stays false and the error pair is
// kept, so the fragment renders the unavailable state rather than an empty list.
func (c APIKeys) loadKeys(ctx context.Context, d *apiKeysData) {
	keys, err := c.API.ListAPIKeys(ctx)
	if err != nil {
		if d.ErrKey == "" {
			d.fail("apikeys.err_list", err.Error())
		}
		return
	}
	rows := make([]apiKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, apiKeyRow{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt})
	}
	d.Keys, d.Count, d.Known = rows, len(rows), true
}

// markCreated tags the row this response just created. The prefix is only known
// here (the key was never stored in plaintext), so it is attached to that row
// for as long as the page carries it.
func (c APIKeys) markCreated(d *apiKeysData) {
	if d.NewID == 0 {
		return
	}
	for i := range d.Keys {
		if d.Keys[i].ID == d.NewID {
			d.Keys[i].Prefix, d.Keys[i].JustCreated = d.TokenPrefix, true
			return
		}
	}
}

// tokenPrefix returns the publishable part of a bearer token: the "pmc_" tag and
// the 8-hex id the daemon indexes keys by. The secret half is never displayed, so
// a token in an unexpected shape is truncated rather than echoed.
func tokenPrefix(tok string) string {
	rest := strings.TrimPrefix(tok, "pmc_")
	if rest != tok {
		if i := strings.IndexByte(rest, '_'); i > 0 {
			return "pmc_" + rest[:i]
		}
	}
	if len(tok) > 12 {
		return tok[:12]
	}
	return tok
}

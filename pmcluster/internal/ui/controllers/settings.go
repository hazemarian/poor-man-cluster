package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
)

// Settings shows and edits the pmcluster API connection.
type Settings struct{ *Controller }

type settingsData struct {
	APIURL      string
	EnvAPIURL   string
	HasToken    bool
	HasEnvToken bool
	Configured  bool
	Version     string
	User        string
	Error       string
	Msg         string
}

// Page renders the settings fragment with current effective values.
func (c Settings) Page(g *gin.Context) {
	apiURL, _, configured := c.loadParams(g.Request.Context())
	d := settingsData{
		APIURL:      apiURL,
		EnvAPIURL:   c.EnvAPI,
		HasEnvToken: c.EnvToken != "",
		Configured:  configured,
		Version:     c.Version,
		User:        username(g),
	}
	if _, err := c.Store.GetSetting(g.Request.Context(), keyToken); err == nil {
		d.HasToken = true
	}
	c.Views.Fragment(g, "settings", d)
}

// Save persists API URL/token overrides and applies them immediately.
func (c Settings) Save(g *gin.Context) {
	ctx := g.Request.Context()
	apiURL := g.PostForm("api_url")
	token := g.PostForm("api_token")
	clearToken := g.PostForm("clear_token") == "1"

	if apiURL == "" {
		// Blank reverts to the env default by removing any override.
		_ = c.Store.SetSetting(ctx, keyAPIURL, "")
		c.API.SetBase(c.EnvAPI)
	} else {
		_ = c.Store.SetSetting(ctx, keyAPIURL, apiURL)
		c.API.SetBase(apiURL)
	}
	switch {
	case clearToken:
		_ = c.Store.SetSetting(ctx, keyToken, "")
		c.API.SetToken(c.EnvToken)
	case token != "" && token != "•••set•••":
		_ = c.Store.SetSetting(ctx, keyToken, token)
		c.API.SetToken(token)
	}
	c.redirectToSettings(g, "Settings saved.")
}

// redirectToSettings reloads the settings fragment with a confirmation message.
func (c Settings) redirectToSettings(g *gin.Context, msg string) {
	apiURL, _, configured := c.loadParams(g.Request.Context())
	d := settingsData{
		APIURL:      apiURL,
		EnvAPIURL:   c.EnvAPI,
		HasEnvToken: c.EnvToken != "",
		Configured:  configured,
		Version:     c.Version,
		User:        username(g),
		Msg:         msg,
	}
	if _, err := c.Store.GetSetting(g.Request.Context(), keyToken); err == nil {
		d.HasToken = true
	}
	c.Views.Fragment(g, "settings", d)
}

func username(g *gin.Context) string {
	if u := middleware.CurrentUser(g); u != nil {
		return u.Username
	}
	return ""
}

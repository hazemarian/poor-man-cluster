package i18n

// English dictionary — sign-in and first-run setup.
//
// Failure keys (auth.err.*) are passed to the template as an i18n key rather
// than a finished sentence, so the controller can distinguish "wrong password"
// from "password too short" without composing English in Go. auth.err.create_failed
// takes a {0} detail (the stored error text).

func init() {
	register(EN, map[string]string{
		// sign in
		"auth.signin_title":    "Sign in",
		"auth.signin":          "Sign in",
		"auth.bad_credentials": "That username and password do not match.",
		"auth.session_note":    "Your session is held in a browser cookie.",

		// sign-in / setup failures
		"auth.err.username_required": "Enter a username.",
		"auth.err.password_short":    "The password needs 8 characters or more.",
		"auth.err.password_mismatch": "The two passwords do not match.",
		"auth.err.create_failed":     "Could not create the admin account: {0}",

		// first run
		"auth.setup_title":         "Set a password",
		"auth.setup_brand":         "first run",
		"auth.setup_intro":         "This is the first run. Create the admin account that unlocks the operator console.",
		"auth.setup_req_admin":     "It is an administrator: users, API keys and secrets all become reachable.",
		"auth.setup_req_password":  "The password needs 8 characters or more.",
		"auth.setup_req_session":   "Signing in issues a 7-day browser session; there is no second factor yet.",
		"auth.setup_password_hint": "8 characters minimum.",
		"auth.setup_confirm":       "Confirm password",
		"auth.setup_create":        "Create admin",
		"auth.setup_done":          "Password set.",
		"auth.setup_done_note":     "Opening the console…",
	})
}

package webui

import "encoding/json"

// settings_view.go holds the hand-written view helpers behind the Settings editor
// (settings.templ, #103). They produce only JS expression strings (for the Alpine
// tab island) — never CSS class strings — so they need no Tailwind content scanning.
// Alpine evaluates the expression at runtime (CSP script-src 'self' 'unsafe-eval').

// settingsTabState returns the Alpine x-data expression for the tabbed editor: it
// seeds the active tab to the first group's id, so a fresh render shows General. An
// empty tab list falls back to an empty active id (the form still renders; no panel
// is shown). The id is JSON-marshaled so it is safely quoted inside the expression.
func settingsTabState(tabs []SettingsTab) string {
	first := ""
	if len(tabs) > 0 {
		first = tabs[0].ID
	}
	lit, err := json.Marshal(first)
	if err != nil {
		lit = []byte(`""`)
	}
	return "{ active: " + string(lit) + " }"
}

// tabSelectedExpr returns the Alpine expression that is true when the given tab is
// active, used for the tab button's aria-selected and active styling binds. Keeping
// it in Go (rather than interpolating into the templ attribute) avoids templ's
// attribute-interpolation rules and keeps the id safely quoted.
func tabSelectedExpr(id string) string {
	lit, err := json.Marshal(id)
	if err != nil {
		lit = []byte(`""`)
	}
	return "active === " + string(lit)
}

// tabActiveClassExpr returns the Alpine class-bind expression that applies the
// active tab class when the tab is selected.
func tabActiveClassExpr(id string) string {
	return tabSelectedExpr(id) + " ? 'settings-tab-active' : ''"
}

// tabSetExpr returns the Alpine click expression that activates the tab.
func tabSetExpr(id string) string {
	lit, err := json.Marshal(id)
	if err != nil {
		lit = []byte(`""`)
	}
	return "active = " + string(lit)
}

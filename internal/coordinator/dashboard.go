package coordinator

import _ "embed"

// dashboardHTML holds the embedded admin dashboard. It lives in its own file so
// the JavaScript template literals (which use backticks) don't collide with Go
// string syntax.
//
//go:embed dashboard.html
var dashboardHTML string

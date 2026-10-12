package apps

import (
	"math"
	"regexp"
)

// Names passed to external tools must not look like flags or contain shell
// or path metacharacters.
var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,254}$`)
	unitRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:\\-]{0,254}$`)
	userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func validName(s string) bool { return nameRe.MatchString(s) }
func validUnit(s string) bool { return unitRe.MatchString(s) }
func validUser(s string) bool { return userRe.MatchString(s) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

package apps

import "time"

// timeNow is wrapped so tests can pin behaviour around expiry/timestamps
// without reaching for a clock interface. The trade-off is the package
// hands `time.Now` indirection at one site instead of every site —
// good enough for what we need today and easy to grow into a real Clock
// later.
var timeNow = func() time.Time { return time.Now() }

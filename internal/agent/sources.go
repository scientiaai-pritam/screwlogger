package agent

// ForegroundSource reports the executable name of the foreground window.
type ForegroundSource interface {
	ForegroundApp() string
}

// IdleSource reports seconds since the last keyboard/mouse input.
type IdleSource interface {
	IdleSeconds() float64
}

// IdleFromTickCount computes idle seconds from GetTickCount-style values,
// safe across uint32 wraparound (~49.7 days of uptime).
func IdleFromTickCount(nowTick, lastInputTick uint32) float64 {
	var delta uint32
	if nowTick >= lastInputTick {
		delta = nowTick - lastInputTick
	} else {
		delta = (0xFFFFFFFF - lastInputTick) + nowTick + 1
	}
	return float64(delta) / 1000.0
}

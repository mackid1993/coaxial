package main

// splicePkts: n packets of a stream whose clock (PCR) is on pcrPID, from clock seconds on, 40 ms apart.
func splicePkts(pcrPID int, clock float64, n int) []byte {
	var out []byte
	for k := 0; k < n; k++ {
		b := int64((clock + 0.04*float64(k)) * 90000)
		p := []byte{0x47, byte(pcrPID >> 8), byte(pcrPID), 0x30, 7, 0x10,
			byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
		for len(p) < 188 {
			p = append(p, 0xff)
		}
		out = append(out, p...)
	}
	return out
}

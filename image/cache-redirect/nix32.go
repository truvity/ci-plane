package main

// nix32Alphabet is Nix's base-32 alphabet: the digits and the lowercase
// letters without e, o, u and t.
const nix32Alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// nix32 encodes b the way Nix prints a hash in base 32 (nix-hash
// --to-base32): least significant bits first, read from the end.
func nix32(b []byte) string {
	n := (len(b)*8 + 4) / 5
	out := make([]byte, 0, n)
	for i := n - 1; i >= 0; i-- {
		bit := i * 5
		j, k := bit/8, uint(bit%8)
		c := int(b[j]) >> k
		if j+1 < len(b) {
			c |= int(b[j+1]) << (8 - k)
		}
		out = append(out, nix32Alphabet[c&0x1f])
	}
	return string(out)
}

// isNix32 reports whether s is n characters of the Nix base-32 alphabet.
func isNix32(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isNix32Char(s[i]) {
			return false
		}
	}
	return true
}

func isNix32Char(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return c != 'e' && c != 'o' && c != 'u' && c != 't'
	}
	return false
}

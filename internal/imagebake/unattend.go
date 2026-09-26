package imagebake

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"html"
	"math/big"
	"text/template"

	"github.com/a1678991/github-qemu-runner/scripts"
)

var unattendTmpl = template.Must(template.New("unattend").Parse(scripts.WindowsUnattend))

// RenderUnattend fills the bake answer file with the one-time
// Administrator password. The value is XML-escaped here; the template
// uses text/template so nothing else is transformed.
func RenderUnattend(adminPassword string) (string, error) {
	var buf bytes.Buffer
	err := unattendTmpl.Execute(&buf, struct{ AdminPassword string }{html.EscapeString(adminPassword)})
	if err != nil {
		return "", fmt.Errorf("render Unattend.xml: %w", err)
	}
	return buf.String(), nil
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// RandomPassword returns n alphanumeric characters from crypto/rand
// with at least one upper, one lower, and one digit (Windows complexity
// policy). Alphanumeric only, so it is safe in XML and command lines.
func RandomPassword(n int) (string, error) {
	if n < 3 {
		return "", fmt.Errorf("password length %d too short", n)
	}
	for {
		b := make([]byte, n)
		var upper, lower, digit bool
		for i := range b {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
			if err != nil {
				return "", err
			}
			c := passwordAlphabet[k.Int64()]
			b[i] = c
			switch {
			case c >= 'A' && c <= 'Z':
				upper = true
			case c >= 'a' && c <= 'z':
				lower = true
			default:
				digit = true
			}
		}
		if upper && lower && digit {
			return string(b), nil
		}
	}
}

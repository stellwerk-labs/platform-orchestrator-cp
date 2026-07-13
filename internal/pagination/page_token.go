package pagination

import (
	"fmt"
	"strings"
)

type PageTokenCodec struct {
	Parts int
}

func (c PageTokenCodec) Generate(parts ...string) string {
	if len(parts) != c.Parts {
		panic(fmt.Sprintf("page token: expected %d parts, got %d", c.Parts, len(parts)))
	}
	return strings.Join(parts, ",")
}

func (c PageTokenCodec) Parse(token string) ([]string, error) {
	if token == "" {
		return make([]string, c.Parts), nil
	}
	parts := strings.Split(token, ",")
	if len(parts) != c.Parts {
		return nil, fmt.Errorf("invalid page token: expected %d parts, got %d", c.Parts, len(parts))
	}
	return parts, nil
}

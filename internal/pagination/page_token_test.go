package pagination

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate(t *testing.T) {
	codec := PageTokenCodec{Parts: 2}
	assert.Equal(t, "abc,123", codec.Generate("abc", "123"))
}

func TestGenerate_panics_on_wrong_part_count(t *testing.T) {
	codec := PageTokenCodec{Parts: 2}
	assert.Panics(t, func() { codec.Generate("one") })
	assert.Panics(t, func() { codec.Generate("one", "two", "three") })
}

func TestParse_empty_token(t *testing.T) {
	codec := PageTokenCodec{Parts: 2}
	parts, err := codec.Parse("")
	require.NoError(t, err)
	assert.Equal(t, []string{"", ""}, parts)
}

func TestParse_valid_token(t *testing.T) {
	codec := PageTokenCodec{Parts: 2}
	parts, err := codec.Parse("abc,123")
	require.NoError(t, err)
	assert.Equal(t, []string{"abc", "123"}, parts)
}

func TestParse_wrong_part_count(t *testing.T) {
	codec := PageTokenCodec{Parts: 2}

	_, err := codec.Parse("one")
	require.Error(t, err)

	_, err = codec.Parse("one,two,three")
	require.Error(t, err)
}

func TestRoundtrip(t *testing.T) {
	codec := PageTokenCodec{Parts: 3}
	token := codec.Generate("a", "b", "c")
	parts, err := codec.Parse(token)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, parts)
}

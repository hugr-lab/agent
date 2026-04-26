package artifacts

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePublishRequest(t *testing.T) {
	cases := []struct {
		name   string
		req    PublishRequest
		inline int64
		want   error
	}{
		{
			name: "ok minimal path source",
			req: PublishRequest{
				CallerSessionID: "sess-1",
				Source:          PublishSource{Path: "/tmp/x"},
				Name:            "n",
				Type:            "txt",
				Description:     "d",
			},
			inline: 1024,
			want:   nil,
		},
		{
			name: "ok inline source",
			req: PublishRequest{
				CallerSessionID: "sess-1",
				Source:          PublishSource{InlineBytes: []byte("hello")},
				Name:            "n",
				Type:            "txt",
				Description:     "d",
			},
			inline: 1024,
			want:   nil,
		},
		{
			name: "missing caller session",
			req: PublishRequest{
				Source: PublishSource{Path: "/x"}, Name: "n", Type: "txt", Description: "d",
			},
			want: errStringMatch("CallerSessionID required"),
		},
		{
			name: "missing name",
			req: PublishRequest{
				CallerSessionID: "s", Source: PublishSource{Path: "/x"}, Type: "txt", Description: "d",
			},
			want: errStringMatch("Name required"),
		},
		{
			name: "missing type",
			req: PublishRequest{
				CallerSessionID: "s", Source: PublishSource{Path: "/x"}, Name: "n", Description: "d",
			},
			want: errStringMatch("Type required"),
		},
		{
			name: "empty description",
			req: PublishRequest{
				CallerSessionID: "s", Source: PublishSource{Path: "/x"}, Name: "n", Type: "txt", Description: "   ",
			},
			want: ErrDescriptionRequired,
		},
		{
			name: "both sources set",
			req: PublishRequest{
				CallerSessionID: "s",
				Source:          PublishSource{Path: "/x", InlineBytes: []byte("y")},
				Name:            "n", Type: "txt", Description: "d",
			},
			want: ErrSourceAmbiguous,
		},
		{
			name: "no source set",
			req: PublishRequest{
				CallerSessionID: "s",
				Source:          PublishSource{},
				Name:            "n", Type: "txt", Description: "d",
			},
			want: ErrSourceAmbiguous,
		},
		{
			name: "inline too large",
			req: PublishRequest{
				CallerSessionID: "s",
				Source:          PublishSource{InlineBytes: []byte(strings.Repeat("a", 16))},
				Name:            "n", Type: "txt", Description: "d",
			},
			inline: 8,
			want:   ErrInlineBytesTooLarge,
		},
		{
			name: "invalid visibility",
			req: PublishRequest{
				CallerSessionID: "s",
				Source:          PublishSource{Path: "/x"},
				Name:            "n", Type: "txt", Description: "d",
				Visibility: Visibility("nope"),
			},
			want: ErrInvalidVisibility,
		},
		{
			name: "invalid ttl",
			req: PublishRequest{
				CallerSessionID: "s",
				Source:          PublishSource{Path: "/x"},
				Name:            "n", Type: "txt", Description: "d",
				TTL: TTL("forever"),
			},
			want: ErrInvalidTTL,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePublishRequest(c.req, c.inline)
			if c.want == nil {
				require.NoError(t, err)
				return
			}
			if sentinel, ok := c.want.(errStringMatch); ok {
				require.Error(t, err)
				assert.Contains(t, err.Error(), string(sentinel))
				return
			}
			require.True(t, errors.Is(err, c.want), "want %v, got %v", c.want, err)
		})
	}
}

// errStringMatch is a sentinel value that signals "match the string,
// not the wrapped error" — used for the messages that don't have
// dedicated sentinels (e.g. "Name required").
type errStringMatch string

func (errStringMatch) Error() string { return "" }

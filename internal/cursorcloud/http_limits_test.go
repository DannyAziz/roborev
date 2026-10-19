package cursorcloud

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultResponseHeaderTimeout(t *testing.T) {
	c, ok := NewHTTPClient("k").(*httpClient)
	require.True(t, ok)
	tr, ok := c.hc.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, defaultResponseHeaderTimeout, tr.ResponseHeaderTimeout)
}

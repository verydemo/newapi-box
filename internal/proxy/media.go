package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/verydemo/newapi-box/relaykit/relayconvert"
	"github.com/verydemo/newapi-box/relaykit/types"
)

// maxMediaBytes bounds how much of a remote image the converter will pull into
// memory when a cross-protocol conversion needs inline base64.
const maxMediaBytes = 20 << 20

// mediaResolver fetches remote and inline media for conversions that must
// inline images (OpenAI Chat -> Claude/Gemini in particular).
//
// relaykit never opens sockets itself: it calls back into the host. A
// converter is a host, so the callbacks live here.
type mediaResolver struct {
	client *http.Client
}

func (m mediaResolver) resolver() relayconvert.MediaResolver {
	return relayconvert.MediaResolver{
		GetBase64Data:        m.getBase64Data,
		DecodeBase64FileData: decodeDataURL,
	}
}

func (m mediaResolver) getBase64Data(ctx context.Context, source types.FileSource, _ ...string) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("media source is nil")
	}
	if !source.IsURL() {
		mimeType := "application/octet-stream"
		if cached := source.GetCache(); cached != nil && cached.MimeType != "" {
			mimeType = cached.MimeType
		}
		return source.GetRawData(), mimeType, nil
	}

	cache := source.GetCache()
	if cache != nil {
		data, err := cache.GetBase64Data()
		if err != nil {
			return "", "", err
		}
		return data, cache.MimeType, nil
	}

	base64Data, mimeType, err := m.fetchAsBase64(ctx, source.GetRawData())
	if err != nil {
		return "", "", err
	}
	source.SetCache(types.NewMemoryCachedData(base64Data, mimeType, int64(len(base64Data))))
	return base64Data, mimeType, nil
}

func (m mediaResolver) fetchAsBase64(ctx context.Context, url string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("build media request: %w", err)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("fetch media: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch media: upstream returned %d", response.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxMediaBytes))
	if err != nil {
		return "", "", fmt.Errorf("read media: %w", err)
	}

	mimeType := response.Header.Get("Content-Type")
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return base64.StdEncoding.EncodeToString(raw), mimeType, nil
}

// decodeDataURL splits a `data:<mime>;base64,<payload>` URL. Inputs without a
// data prefix are treated as raw base64 with an unknown mime type.
func decodeDataURL(dataURL string) (string, string, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return dataURL, "application/octet-stream", nil
	}
	header, payload, found := strings.Cut(strings.TrimPrefix(dataURL, "data:"), ",")
	if !found {
		return "", "", fmt.Errorf("malformed data URL: missing payload separator")
	}
	mimeType, _, _ := strings.Cut(header, ";")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return payload, mimeType, nil
}

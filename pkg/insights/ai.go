package insights

import (
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
)

func getenv(k string) string { return os.Getenv(k) }

type aiCfg struct{ key, model, base string }

func aiConfig() aiCfg {
	c := assistant.ConfigFromEnv()
	return aiCfg{key: c.APIKey, model: c.Model, base: c.BaseURL}
}

func ioCopy(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, io.LimitReader(src, 2<<20))
}

func newID() string { return uuid.NewString() }

package fingerprint

import (
	"fmt"

	"github.com/go-task/task/v3/internal/logger"
)

func NewSourcesChecker(method, tempDir string, dry bool, l *logger.Logger) (SourcesCheckable, error) {
	switch method {
	case "timestamp":
		return NewTimestampChecker(tempDir, dry, l), nil
	case "checksum":
		return NewChecksumChecker(tempDir, dry, l), nil
	case "none":
		return NoneChecker{}, nil
	default:
		return nil, fmt.Errorf(`task: invalid method "%s"`, method)
	}
}

package scheduler

import (
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

// Provider provides a schedule.
type Provider interface {
	Provide() (Schedule, error)
}

// MemProvider provides a schedule from given Schedule.
type MemProvider struct {
	Schedule Schedule
}

func (mp *MemProvider) Provide() (Schedule, error) {
	return mp.Schedule, nil
}

// FileProvider provides a schedule from a file.
type FileProvider struct {
	Filename string
}

func (fp *FileProvider) Provide() (Schedule, error) {
	if strings.Contains(fp.Filename, "../") || strings.Contains(fp.Filename, "..\\") {
		return nil, errors.Errorf("Invalid file path: %s", fp.Filename)
	}
	file, err := os.Open(fp.Filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Provide(file)
}

// Provide obtains a schedule from a r.
func Provide(r io.Reader) (Schedule, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var sc Schedule
	if err = yaml.UnmarshalStrict(data, &sc); err != nil {
		return nil, err
	}
	return sc, nil
}

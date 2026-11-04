package commands

import (
	"github.com/Mag1cFall/wails/v3/internal/templates"
)

func GenerateTemplate(options *templates.BaseTemplate) error {
	return templates.GenerateTemplate(options)
}

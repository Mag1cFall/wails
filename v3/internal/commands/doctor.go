package commands

import (
	"github.com/Mag1cFall/wails/v3/internal/doctor"
	"github.com/Mag1cFall/wails/v3/internal/flags"
)

func Doctor(options *flags.Doctor) error {
	return doctor.Run(options.JSON)
}

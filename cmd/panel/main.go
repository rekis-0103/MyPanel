package main

import (
	"log"

	"mypanel/internal/app"
)

var (
	version = "dev"
	commit  = "local"
	date    = "unknown"
)

func main() {
	if err := app.Run(app.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	}); err != nil {
		log.Fatal(err)
	}
}

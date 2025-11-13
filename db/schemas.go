package db

import (
	"embed"
)

//go:embed sqlc/migrations/*.*.sql
var sqlSchemas embed.FS

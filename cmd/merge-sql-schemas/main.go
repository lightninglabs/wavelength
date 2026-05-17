package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	_ "modernc.org/sqlite"
)

// applyMigrationDir executes all .up.sql migrations in lexicographic order for
// the given directory.
func applyMigrationDir(db *sql.DB, migrationDir string) error {
	files, err := os.ReadDir(migrationDir)
	if err != nil {
		return fmt.Errorf("failed to read migration directory %s: %w",
			migrationDir, err)
	}

	var migrationFiles []string
	upSQLPattern := regexp.MustCompile(`\.up\.sql$`)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if upSQLPattern.MatchString(file.Name()) {
			migrationFiles = append(migrationFiles, file.Name())
		}
	}
	sort.Strings(migrationFiles)

	for _, fileName := range migrationFiles {
		filePath := filepath.Join(migrationDir, fileName)
		// Dev-only build tool reading migration files from a
		// caller-controlled directory; no external input.
		content, err := os.ReadFile(filePath) //nolint:gosec // G304
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w",
				filePath, err)
		}

		_, err = db.Exec(string(content))
		if err != nil {
			return fmt.Errorf("failed to execute migration %s: %w",
				filePath, err)
		}

		log.Printf("Executed migration: %s", filePath)
	}

	return nil
}

func main() {
	// Open an in-memory SQLite database.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		_ = db.Close()
	}()

	migrationDirs := []string{
		"db/sqlc/migrations",
	}

	for _, migrationDir := range migrationDirs {
		err = applyMigrationDir(db, migrationDir)
		if err != nil {
			//nolint:gocritic
			log.Fatalf("Failed to apply migrations for %s: %v",
				migrationDir, err)
		}
	}

	// Query the sqlite_master table to extract the schema.
	query := `
		SELECT type, name, sql
		FROM sqlite_master
		WHERE type IN ('table','view', 'index')
			AND sql IS NOT NULL
			AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`

	rows, err := db.Query(query)
	if err != nil {
		log.Fatalf("Failed to query schema: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	// Build the consolidated schema.
	var schema string
	for rows.Next() {
		var objType, name, sqlStmt string
		err := rows.Scan(&objType, &name, &sqlStmt)
		if err != nil {
			log.Fatalf("Failed to scan row: %v", err)
		}

		schema += sqlStmt + ";\n\n"
	}

	if err = rows.Err(); err != nil {
		log.Fatalf("Error iterating rows: %v", err)
	}

	// Write the schema to the output file. This is a build-time tool
	// writing into the project tree, not a runtime permission.
	outDir := "db/sqlc/schemas"
	err = os.MkdirAll(outDir, 0755) //nolint:gosec // G301
	if err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	outFile := filepath.Join(outDir, "generated_schema.sql")
	err = os.WriteFile(outFile, []byte(schema), 0644)
	if err != nil {
		log.Fatalf("Failed to write schema file: %v", err)
	}

	log.Printf("Successfully generated schema at: %s", outFile)
}

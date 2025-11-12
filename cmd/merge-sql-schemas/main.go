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

func main() {
	// Open an in-memory SQLite database.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Read all migration files from db/sqlc/migrations/.
	migrationDir := "db/sqlc/migrations"
	files, err := os.ReadDir(migrationDir)
	if err != nil {
		log.Fatalf("Failed to read migration directory: %v", err)
	}

	// Filter for .up.sql files and sort them.
	var migrationFiles []string
	upSqlPattern := regexp.MustCompile(`\.up\.sql$`)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if upSqlPattern.MatchString(file.Name()) {
			migrationFiles = append(migrationFiles, file.Name())
		}
	}
	sort.Strings(migrationFiles)

	// Execute each migration in order.
	for _, fileName := range migrationFiles {
		filePath := filepath.Join(migrationDir, fileName)
		content, err := os.ReadFile(filePath)
		if err != nil {
			log.Fatalf("Failed to read file %s: %v", fileName, err)
		}

		_, err = db.Exec(string(content))
		if err != nil {
			log.Fatalf("Failed to execute migration %s: %v",
				fileName, err)
		}

		log.Printf("Executed migration: %s", fileName)
	}

	// Query the sqlite_master table to extract the schema.
	query := `
		SELECT type, name, sql
		FROM sqlite_master
		WHERE type IN ('table','view', 'index') AND sql IS NOT NULL
		ORDER BY name
	`

	rows, err := db.Query(query)
	if err != nil {
		log.Fatalf("Failed to query schema: %v", err)
	}
	defer rows.Close()

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

	// Write the schema to the output file.
	outDir := "db/sqlc/schemas"
	err = os.MkdirAll(outDir, 0755)
	if err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	outFile := filepath.Join(outDir, "generated_schema.sql")
	err = os.WriteFile(outFile, []byte(schema), 0644)
	if err != nil {
		log.Fatalf("Failed to write schema file: %v", err)
	}

	fmt.Printf("Successfully generated schema at: %s\n", outFile)
}

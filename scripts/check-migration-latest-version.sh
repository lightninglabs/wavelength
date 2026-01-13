#!/bin/bash

set -e

# Get the latest version number from the migration file names.
migrations_path="db/sqlc/migrations"
latest_file_version=$(ls -r $migrations_path | grep .up.sql | head -1 | cut -d_ -f1)

# Force base 10 interpretation, getting rid of the leading zeroes.
latest_file_version=$((10#$latest_file_version))

# Check the value in migrations.go. Use sed for portability (macOS/Linux).
file_path="db/migrations.go"
latest_code_version=$(grep 'LatestMigrationVersion' "$file_path" | grep -o '[0-9]\+' | head -1)

if [ "$latest_file_version" -ne "$latest_code_version" ]; then
    echo "ERROR: Migration version mismatch!"
    echo "Latest migration version in file names: $latest_file_version"
    echo "Latest migration version in code: $latest_code_version"
    echo ""
    echo "Please update LatestMigrationVersion in $file_path to $latest_file_version"
    exit 1
fi

echo "Migration version check passed: $latest_file_version"

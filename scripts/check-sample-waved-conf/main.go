package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/waved"
)

const (
	defaultConfFile = "sample-waved.conf"
	mainFile        = "cmd/waved/main.go"
)

var skippedFlags = map[string]struct{}{
	"configfile": {},
	"help":       {},
	"version":    {},
}

// main verifies that sample-waved.conf documents every daemon config option
// with the current default value.
func main() {
	confFile := defaultConfFile
	if len(os.Args) > 1 && os.Args[1] != "" {
		confFile = os.Args[1]
	}

	expected, err := expectedConfigKeys()
	if err != nil {
		fail(err)
	}

	if err := addDaemonFlagKeys(expected); err != nil {
		fail(err)
	}

	sample, err := parseSampleConfig(confFile)
	if err != nil {
		fail(err)
	}

	if err := checkSampleConfig(expected, sample); err != nil {
		fail(err)
	}

	_, _ = fmt.Fprintf(
		os.Stdout, "%d sample waved config options checked\n",
		len(expected),
	)
}

// expectedConfigKeys returns the default daemon config keys derived from
// mapstructure tags.
func expectedConfigKeys() (map[string]string, error) {
	cfg := waved.DefaultConfig()
	expected := make(map[string]string)

	if err := collectConfigKeys(
		"", reflect.ValueOf(cfg), expected,
	); err != nil {
		return nil, err
	}

	return expected, nil
}

// collectConfigKeys recursively collects mapstructure-tagged fields from a
// config struct.
func collectConfigKeys(prefix string, value reflect.Value,
	expected map[string]string) error {

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			value = reflect.Zero(value.Type().Elem())
		} else {
			value = value.Elem()
		}
	}

	if value.Kind() != reflect.Struct {
		return nil
	}

	valueType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		field := valueType.Field(i)
		if !field.IsExported() {
			continue
		}

		tag := strings.Split(field.Tag.Get("mapstructure"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}

		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}

		fieldValue := value.Field(i)
		fieldKind := fieldValue.Kind()
		if fieldKind == reflect.Pointer &&
			fieldValue.Type().Elem().Kind() == reflect.Struct {

			if err := collectConfigKeys(key, fieldValue,
				expected); err != nil {
				return err
			}

			continue
		}
		if fieldKind == reflect.Struct &&
			fieldValue.Type() != reflect.TypeOf(time.Duration(0)) {

			if err := collectConfigKeys(key, fieldValue,
				expected); err != nil {
				return err
			}

			continue
		}

		defaultValue, err := formatDefaultValue(fieldValue)
		if err != nil {
			return fmt.Errorf("format default for %s: %w", key, err)
		}

		expected[key] = defaultValue
	}

	return nil
}

// formatDefaultValue converts a default config value to the representation used
// in sample-waved.conf.
func formatDefaultValue(value reflect.Value) (string, error) {
	if value.Type() == reflect.TypeOf(time.Duration(0)) {
		duration, ok := value.Interface().(time.Duration)
		if !ok {
			return "", errors.New("expected time.Duration")
		}

		return duration.String(), nil
	}

	switch value.Kind() {
	case reflect.String:
		return value.String(), nil

	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64:
		return strconv.FormatUint(value.Uint(), 10), nil

	case reflect.Slice:
		if value.Len() == 0 {
			return "", nil
		}

		parts := make([]string, value.Len())
		for i := 0; i < value.Len(); i++ {
			part, err := formatDefaultValue(value.Index(i))
			if err != nil {
				return "", err
			}
			parts[i] = part
		}

		return strings.Join(parts, ","), nil

	default:
		return "", fmt.Errorf("unsupported kind %s", value.Kind())
	}
}

// addDaemonFlagKeys adds daemon CLI-only config keys and verifies source flags
// appear in waved help output.
func addDaemonFlagKeys(expected map[string]string) error {
	sourceBytes, err := os.ReadFile(mainFile)
	if err != nil {
		return err
	}
	source := string(sourceBytes)

	flagAliases := explicitFlagAliases(source)
	helpFlags, err := daemonHelpFlags()
	if err != nil {
		return err
	}

	flagDefaults := map[string]string{}
	// First capture default values from the user-facing help output. This
	// also adds daemon-only flags, such as bitcoind.*, that are not present
	// in waved.Config.
	for _, flag := range helpFlags {
		if _, ok := skippedFlags[flag.name]; ok {
			continue
		}

		key := flag.name
		if alias, ok := flagAliases[flag.name]; ok {
			key = alias
		}

		if _, ok := expected[key]; ok {
			continue
		}

		expected[key] = flag.defaultValue
		flagDefaults[key] = flag.defaultValue
	}

	registeredFlags := registeredDaemonFlags(source)
	// Then verify every source-registered flag appeared in help. Keeping
	// this as a separate pass catches parser drift without losing the help
	// defaults collected above.
	for _, flagName := range registeredFlags {
		if _, ok := skippedFlags[flagName]; ok {
			continue
		}

		key := flagName
		if alias, ok := flagAliases[flagName]; ok {
			key = alias
		}

		if _, ok := expected[key]; ok {
			continue
		}

		defaultValue, ok := flagDefaults[key]
		if !ok {
			return fmt.Errorf("daemon flag %q was registered but "+
				"not found in waved --help", flagName)
		}

		expected[key] = defaultValue
	}

	return nil
}

// explicitFlagAliases returns config-key aliases from explicit Viper flag
// bindings.
func explicitFlagAliases(source string) map[string]string {
	re := regexp.MustCompile(
		`(?s)v\.BindPFlag\(\s*"([^"]+)"\s*,` +
			`\s*f\.Lookup\(\s*"([^"]+)"\s*\)`,
	)
	matches := re.FindAllStringSubmatch(source, -1)

	aliases := make(map[string]string, len(matches))
	for _, match := range matches {
		configKey, flagName := match[1], match[2]
		aliases[flagName] = configKey
	}

	return aliases
}

// registeredDaemonFlags returns daemon flag names registered with literal
// string arguments.
func registeredDaemonFlags(source string) []string {
	// This checker assumes daemon flags are registered in
	// cmd/waved/main.go with literal string names. If flags move behind
	// helpers or variables, update this parser alongside that refactor.
	re := regexp.MustCompile(
		`(?s)\bf\.(?:String|Bool|Int|Int32|Int64|Uint|` +
			`Uint32|Uint64|Duration|StringSlice|` +
			`StringToString)\(\s*"([^"]+)"`,
	)
	matches := re.FindAllStringSubmatch(source, -1)

	flags := make([]string, 0, len(matches))
	for _, match := range matches {
		flags = append(flags, match[1])
	}

	return flags
}

type daemonFlag struct {
	name         string
	defaultValue string
}

// daemonHelpFlags returns the daemon flags and defaults from waved --help.
func daemonHelpFlags() ([]daemonFlag, error) {
	cmd := exec.Command("go", "run", "./cmd/waved", "--help")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run waved --help: %w\n%s", err,
			string(output))
	}

	re := regexp.MustCompile(
		`^\s+(?:-[A-Za-z],\s+)?--([A-Za-z0-9_.-]+)` +
			`(?:\s+([A-Za-z0-9]+))?.*$`,
	)
	defaultRe := regexp.MustCompile(`\(default (?:"([^"]*)"|([^)]*))\)`)

	var flags []daemonFlag
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		match := re.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		name := match[1]
		valueType := match[2]
		defaultValue := ""
		if defaultMatch := defaultRe.FindStringSubmatch(
			line,
		); defaultMatch != nil {

			defaultValue = defaultMatch[1]
			if defaultValue == "" {
				defaultValue = strings.TrimSpace(
					defaultMatch[2],
				)
			}
		} else if valueType == "" {
			defaultValue = "false"
		}

		flags = append(flags, daemonFlag{
			name:         name,
			defaultValue: defaultValue,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return flags, nil
}

// parseSampleConfig returns the commented key/value entries from the sample
// config file.
func parseSampleConfig(confFile string) (map[string]string, error) {
	file, err := os.Open(confFile) //nolint:gosec // G304: CI input path.
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	re := regexp.MustCompile(`^\s*#\s*([A-Za-z0-9_.-]+)=(.*)$`)
	sample := make(map[string]string)

	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" && !strings.HasPrefix(trimmedLine, "#") {
			return nil, fmt.Errorf("%s:%d contains live config "+
				"line %q; sample entries must stay commented",
				confFile, lineNumber, trimmedLine)
		}

		match := re.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		key, value := match[1], strings.TrimRight(match[2], " \t")
		if _, ok := sample[key]; ok {
			return nil, fmt.Errorf("%s:%d duplicates %q", confFile,
				lineNumber, key)
		}

		sample[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return sample, nil
}

// checkSampleConfig verifies the sample keys and defaults match expectations.
func checkSampleConfig(expected, sample map[string]string) error {
	var missing []string
	for key := range expected {
		if _, ok := sample[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)

	var stale []string
	for key := range sample {
		if _, ok := expected[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)

	var mismatched []string
	for key, expectedValue := range expected {
		sampleValue, ok := sample[key]
		if !ok || sampleValue == expectedValue {
			continue
		}

		mismatched = append(
			mismatched, fmt.Sprintf("%s: sample %q, default %q",
				key, sampleValue, expectedValue),
		)
	}
	sort.Strings(mismatched)

	if len(missing) == 0 && len(stale) == 0 && len(mismatched) == 0 {
		return nil
	}

	var builder strings.Builder
	if len(missing) > 0 {
		fmt.Fprintf(&builder, "missing sample config keys:\n")
		for _, key := range missing {
			fmt.Fprintf(&builder, "  - %s\n", key)
		}
	}
	if len(stale) > 0 {
		fmt.Fprintf(&builder, "unknown sample config keys:\n")
		for _, key := range stale {
			fmt.Fprintf(&builder, "  - %s\n", key)
		}
	}
	if len(mismatched) > 0 {
		fmt.Fprintf(&builder, "sample defaults differ:\n")
		for _, mismatch := range mismatched {
			builder.WriteString("  - ")
			builder.WriteString(mismatch)
			builder.WriteByte('\n')
		}
	}

	return errors.New(strings.TrimSpace(builder.String()))
}

// fail reports an error and exits the checker with a non-zero status.
func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

package linters

import (
	"github.com/golangci/plugin-module-register/register"
	"github.com/lightninglabs/wavelength/tools/linters/actorblock"
	"golang.org/x/tools/go/analysis"
)

const actorBlockName = "actorblock"

// ActorBlockConfig is the configuration for the actorblock linter.
type ActorBlockConfig struct {
	// ActorPkg is the import path of the actor framework. It defaults to
	// the wavelength baselib actor package.
	ActorPkg string `json:"actor-pkg"`

	// EntryMethods lists extra entry points as "Method@pkgpath.Interface".
	// It defaults to the protofsm State.ProcessEvent and
	// ActorOutboxEvent.Dispatch methods.
	EntryMethods []string `json:"entry-methods"`

	// Baseline is the path of the baseline file listing functions with
	// tolerated legacy blocking sites.
	Baseline string `json:"baseline"`
}

// NewActorBlock creates the actorblock plugin from the given settings. It
// satisfies the signature required by golangci-lint for plugins.
func NewActorBlock(settings any) (register.LinterPlugin, error) {
	cfg, err := register.DecodeSettings[ActorBlockConfig](settings)
	if err != nil {
		return nil, err
	}

	return &ActorBlockPlugin{cfg: cfg}, nil
}

// ActorBlockPlugin is a golangci-lint plugin that reports actor turns that
// can block, see the actorblock package.
type ActorBlockPlugin struct {
	cfg ActorBlockConfig
}

// BuildAnalyzers creates the analyzers for the actorblock linter.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (p *ActorBlockPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{
		actorblock.NewAnalyzer(&actorblock.Config{
			ActorPkg:     p.cfg.ActorPkg,
			EntryMethods: p.cfg.EntryMethods,
			BaselinePath: p.cfg.Baseline,
		}),
	}, nil
}

// GetLoadMode returns the load mode for the actorblock linter. It needs type
// information, and the analyzer's facts carry the call graph across packages.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (p *ActorBlockPlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

func init() {
	// Register the linter with the plugin module register.
	register.Plugin(actorBlockName, NewActorBlock)
}

# Testing flags and configuration for darepo.

# Default test flags.
TEST_FLAGS :=

# Default timeout is 180m (3 hours), matching lnd's default.
DEFAULT_TIMEOUT := 180m

# If specific package is being unit tested, construct the full name of the
# subpackage.
ifneq ($(pkg),)
UNITPKG := $(PKG)/$(pkg)
UNIT_TARGETED = yes
COVER_PKG = $(PKG)/$(pkg)
endif

# If a specific unit test case is being targeted, construct test.run filter.
ifneq ($(case),)
TEST_FLAGS += -test.run=$(case)
UNIT_TARGETED = yes
endif

# Define the log tags that will be applied only when running unit tests. If
# none are provided, we default to "nolog" which will be silent.
ifneq ($(log),)
LOG_TAGS := ${log}
else
LOG_TAGS := nolog
endif

# If a timeout was requested, construct the proper flag for the go test
# command. If not, we set the default timeout.
ifneq ($(timeout),)
TEST_FLAGS += -test.timeout=$(timeout)
else
TEST_FLAGS += -test.timeout=$(DEFAULT_TIMEOUT)
endif

# Add verbose flag if requested.
ifneq ($(verbose),)
TEST_FLAGS += -test.v
endif

# Add count=1 to disable test caching if requested.
ifneq ($(nocache),)
TEST_FLAGS += -test.count=1
endif

# If the short flag is added, then any unit tests marked with "testing.Short()"
# will be skipped.
ifneq ($(short),)
TEST_FLAGS += -short
endif

# UNIT_TARGETED is undefined iff a specific package and/or unit test case is
# not being targeted.
UNIT_TARGETED ?= no

# GOLIST lists all packages in the project, excluding vendor and generated code.
GOLIST := $(GOCC) list -tags="$(DEV_TAGS)" -deps $(PKG)/... | grep '$(PKG)' | grep -v '/vendor/'

# If a specific package/test case was requested, run the unit test for the
# targeted case. Otherwise, default to running all tests.
ifeq ($(UNIT_TARGETED), yes)
UNIT := $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(UNITPKG)
UNIT_DEBUG := $(GOTEST) -v -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(UNITPKG)
UNIT_RACE := $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) -race $(UNITPKG)
UNIT_COVER := $(GOTEST) $(COVER_FLAGS) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(COVER_PKG)
endif

ifeq ($(UNIT_TARGETED), no)
UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS)
UNIT_DEBUG := $(GOLIST) | $(XARGS) env $(GOTEST) -v -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS)
UNIT_RACE := $(UNIT) -race
UNIT_COVER := $(GOTEST) $(COVER_FLAGS) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(COVER_PKG)
endif

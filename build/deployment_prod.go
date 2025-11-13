// Copyright (C) 2015-2024 Lightning Labs and The Lightning Network Developers

//go:build !dev
// +build !dev

package build

// Deployment is the current deployment mode of the software. By default, this
// is set to Production unless the "dev" build tag is specified.
const Deployment = Production

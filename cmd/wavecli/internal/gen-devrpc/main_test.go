package main

import (
	"testing"
)

func TestCollectServicesIncludesExpectedRPCs(t *testing.T) {
	services, err := collectServices()
	if err != nil {
		t.Fatalf("collect services: %v", err)
	}

	if len(services) != len(expectedServices) {
		t.Fatalf("service count = %v, want %v", len(services),
			len(expectedServices))
	}

	var sawDaemon, sawSwap, sawBtcwalletVersion, sawBtcwallet bool
	for _, service := range services {
		switch service.FullName {
		case "waverpc.DaemonService":
			sawDaemon = true
			if service.Comments == "" {
				t.Fatalf("daemon service has no comments")
			}
			if len(service.Methods) == 0 {
				t.Fatalf("daemon service has no methods")
			}
			assertMethodComment(t, service, "GetInfo")

		case "swapclientrpc.SwapClientService":
			sawSwap = true
			if service.Comments == "" {
				t.Fatalf("swap service has no comments")
			}
			if len(service.Methods) == 0 {
				t.Fatalf("swap service has no methods")
			}
			assertMethodComment(t, service, "StartPay")

		case "walletrpc.VersionService":
			sawBtcwalletVersion = true
			assertMethodExists(t, service, "Version")

		case "walletrpc.WalletService":
			sawBtcwallet = true
			assertMethodExists(t, service, "NextAddress")
			assertMethodExists(t, service, "FundTransaction")
		}
	}

	if !sawDaemon || !sawSwap || !sawBtcwalletVersion ||
		!sawBtcwallet {

		t.Fatalf("missing expected service, got %#v", services)
	}
}

func assertMethodComment(t *testing.T, service serviceData, name string) {
	t.Helper()

	for _, method := range service.Methods {
		if method.Name != name {
			continue
		}

		if method.Comments == "" {
			t.Fatalf("%s has no comments", name)
		}

		return
	}

	t.Fatalf("method %s not found", name)
}

func assertMethodExists(t *testing.T, service serviceData, name string) {
	t.Helper()

	for _, method := range service.Methods {
		if method.Name == name {
			return
		}
	}

	t.Fatalf("method %s not found", name)
}

func TestNormalizeInitialismAliases(t *testing.T) {
	got := camelToKebab(normalizeInitialisms("ListVTXOs"))
	if got != "list-vtxos" {
		t.Fatalf("ListVTXOs alias = %q", got)
	}

	got = camelToKebab(normalizeInitialisms("ReceiveAuthECDH"))
	if got != "receive-auth-ecdh" {
		t.Fatalf("ReceiveAuthECDH alias = %q", got)
	}
}

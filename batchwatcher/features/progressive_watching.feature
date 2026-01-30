Feature: Progressive Tree Watching
  As a BatchWatcher
  I want to watch tree outputs progressively
  So that I efficiently track only unrolled paths

  Scenario: Batch output spend triggers child watching
    Given a BatchWatcher with a registered batch for watching
    When the batch output is spent
    Then child outputs should be registered for watching
    And BatchSweeper should get TreeStateChanged

  Scenario: VTXO leaf appears on-chain
    Given a BatchWatcher with a registered batch for watching
    When a spend reveals VTXO leaf outputs
    Then FraudDetector should get VTXOOnChainNotification
    And no further watch should be registered for the VTXO

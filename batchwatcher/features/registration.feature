Feature: Batch Registration
  As an Ark operator
  I want to register batches for monitoring
  So that I can track on-chain tree state and notify child actors

  Scenario: Register a new batch successfully
    Given a BatchWatcher with no registered batches
    When I register a batch with expiry height 1000
    Then the batch should be tracked in state
    And a spend watch should be registered on batch output

  Scenario: Query tree state for registered batch
    Given a BatchWatcher with a registered batch
    When I query the tree state
    Then the response should indicate found

  Scenario: Query tree state for unknown batch
    Given a BatchWatcher with no registered batches
    When I query tree state for an unknown batch
    Then the response should indicate not found

  Scenario: Unregister a batch
    Given a BatchWatcher with a registered batch
    When I unregister the batch
    Then the batch should no longer be tracked

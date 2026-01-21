Feature: Batch expiry triggers sweeping
  As a BatchSweeper
  I want to query tree state when a batch expires
  So that I can decide which outputs to sweep

  Scenario: Expired batch triggers tree state query
    Given a BatchSweeper with watcher reporting the batch exists
    When BatchWatcher notifies a batch expiry
    Then BatchWatcher should be queried for the batch tree state

  Scenario: Expiry for unknown batch still queries watcher
    Given a BatchSweeper with watcher reporting the batch does not exist
    When BatchWatcher notifies a batch expiry
    Then BatchWatcher should be queried for the batch tree state
    And no BatchSweeper error should occur


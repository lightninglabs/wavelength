Feature: Sweeping mature outputs
  As a BatchSweeper
  I want to broadcast sweep transactions for CSV-mature operator outputs
  So that operator funds are reclaimed after batch expiry

  Scenario: Mature operator output triggers broadcast
    Given a BatchSweeper with a mature operator output
    When BatchWatcher notifies a batch expiry
    Then a sweep transaction should be broadcast

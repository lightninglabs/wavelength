Feature: Batch Expiry Notifications
  As a BatchSweeper
  I want to receive notifications when batches expire
  So that I can sweep unspent outputs

  Scenario: Batch expires at block height
    Given a BatchWatcher with batch expiring at height 1000
    When block 1000 is received
    Then BatchSweeper should get BatchExpiredNotification

  Scenario: Multiple batches expire at same height
    Given a BatchWatcher with 2 batches expiring at 1000
    When block 1000 is received
    Then BatchSweeper should receive 2 notifications

  Scenario: No batches expire before expiry height
    Given a BatchWatcher with batch expiring at height 2000
    When block 1000 is received
    Then BatchSweeper should not receive any notifications

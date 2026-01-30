Feature: Unroller - VTXO Tree Unrolling
  As a VTXO client
  I want to unroll VTXO trees on-chain
  So that I can make my VTXOs spendable when needed

  Background:
    Given an unroller actor is running
    And the chain source is available
    And the unroll store is available


  Scenario: Successfully unroll a 2-level VTXO tree
    Given a VTXO with a 2-level tree
    When I request unroll for the VTXO
    Then the unroll should start successfully
    And level 1 transactions should be broadcast
    And the unroll status should be "broadcasting"
    When level 1 transactions confirm
    Then the unroll status should be "awaiting_csv"
    When the CSV delay is satisfied
    Then the unroll status should be "complete"
    And the VTXO should be ready for sweeping

  Scenario: Successfully unroll a multi-level VTXO tree
    Given a VTXO with a 4-level tree
    When I request unroll for the VTXO
    Then the unroll should start successfully
    And level 1 transactions should be broadcast
    When level 1 transactions confirm
    Then the unroll status should be "broadcasting"
    And the actor should wait for CSV delay
    When the CSV delay for level 1 is satisfied
    Then level 2 transactions should be broadcast
    When level 2 transactions confirm
    And the CSV delay for level 2 is satisfied
    Then level 3 transactions should be broadcast
    When level 3 transactions confirm
    Then the unroll status should be "awaiting_csv"
    When the final CSV delay is satisfied
    Then the unroll status should be "complete"

  Scenario: Handle duplicate unroll requests
    Given a VTXO with a 2-level tree
    And an unroll is already in progress for the VTXO
    When I request unroll for the same VTXO again
    Then the duplicate request should be ignored
    And only one unroll should be active

  Scenario: Query unroll status during broadcast
    Given a VTXO with a multi-level tree
    And an unroll is in progress at level 2
    When I query the unroll status
    Then the status should show "broadcasting"
    And the current level should be 2
    And the total levels should be 4

  Scenario: Recover unroll state after restart
    Given a VTXO with a multi-level tree
    And an unroll is in progress at level 2
    When the unroller restarts
    Then the unroll should resume from level 2
    And confirmation subscriptions should be re-registered
    And block epoch subscriptions should be re-registered

  Scenario: Handle broadcast failures gracefully
    Given a VTXO with a 2-level tree
    When I request unroll for the VTXO
    And the broadcast fails for level 1 transaction
    Then the actor should log the failure
    And the unroll should remain in "broadcasting" status
    And retry should be attempted on next block

  Scenario: Unroll multiple VTXOs concurrently
    Given 3 VTXOs with 2-level trees
    When I request unroll for all 3 VTXOs
    Then all 3 unrolls should start successfully
    And each unroll should be tracked independently
    When transactions for VTXO 1 confirm
    Then only VTXO 1 should progress to "awaiting_csv"
    And VTXO 2 and VTXO 3 should remain in "broadcasting"

  Scenario: Validate CSV delay enforcement
    Given a VTXO with CSV delay of 144 blocks
    And the leaf transaction confirmed at height 1000
    When the current block height is 1143
    Then the unroll status should still be "awaiting_csv"
    When the current block height reaches 1144
    Then the unroll status should transition to "complete"

  Scenario: Handle missing VTXO in database
    Given a VTXO outpoint that does not exist in the database
    When I request unroll for the non-existent VTXO
    Then the request should fail with "fetch VTXO" error
    And no unroll should be created

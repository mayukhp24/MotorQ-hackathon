Feature: Tenant isolation and privacy
  As a platform serving competing fleets
  I want every organisation to see only its own data, with personal data minimised by role
  So that customers can trust the platform with their operations

  Scenario: A user cannot read another organisation's vehicle
    Given I am signed in as "admin@greenfleet.demo"
    When I request a vehicle that belongs to "Acme Logistics"
    Then the API answers 404

  Scenario: Analysts see coarse locations and cannot change alerts
    Given I am signed in as "analyst@acme.demo"
    When I open the vehicle list
    Then vehicle positions are snapped to the privacy grid
    And acknowledging an alert is forbidden

  Scenario: Every data access is audited in a tamper-evident chain
    Given I am signed in as "admin@acme.demo"
    When I open the vehicle list
    Then my access appears in the audit trail
    And the audit hash chain verifies as intact

Feature: Real-time critical alerts
  As a maintenance manager
  I want engine overheating detected within seconds of it happening
  So that the driver is told to stop before the engine is damaged

  Scenario: An overheating vehicle raises a critical alert within 5 seconds
    Given I am signed in as "maint@acme.demo"
    And a vehicle from my fleet reported by the "aurora" OEM cloud
    When the OEM cloud reports 20 seconds of coolant at 118 °C for that vehicle
    Then an "ENGINE_OVERHEAT" alert for that vehicle is visible within 5 seconds
    And the alert has severity "CRITICAL"

  Scenario: Malformed records are rejected without losing the valid ones
    When the "pinnacle" OEM cloud sends a batch with 1 valid record and 1 record with a bad VIN check digit
    Then the gateway accepts 1 record and rejects 1 record

  Scenario: Connectors must authenticate
    When the "stellar" OEM cloud sends a batch with a wrong API key
    Then the gateway answers 401

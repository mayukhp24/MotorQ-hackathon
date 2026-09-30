Feature: Predictive maintenance
  As a maintenance manager
  I want to know which vehicles are likely to break down in the next 7 days and why
  So that I can repair them in the workshop instead of at the roadside

  Scenario: Riskiest vehicles are ranked with reasons and a dollar impact
    Given I am signed in as "maint@acme.demo"
    When I open the breakdown predictions
    Then vehicles are ordered by risk, highest first
    And each high-risk vehicle shows at least one reason and an avoidable cost

  Scenario: Scheduling a repair from a prediction
    Given I am signed in as "maint@acme.demo"
    When I schedule a repair for the riskiest vehicle without an open work order
    Then a work order from source "PREDICTION" exists for that vehicle

  Scenario: The copilot can only propose; a manager approves
    Given I am signed in as "maint@acme.demo"
    When I ask the copilot to schedule a repair for a high-risk vehicle
    Then the copilot proposes a work order that is pending approval
    And a viewer cannot approve it
    And when I approve it a work order is created

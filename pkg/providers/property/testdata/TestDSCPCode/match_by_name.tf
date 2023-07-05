provider "akamai" {
  edgerc = "../../test/edgerc"
}

data "akamai_cp_code" "test" {
  name        = "test cpcode"
  contract_id = "ctr_test"
  group_id    = "grp_test"
}

CREATE OR REPLACE FUNCTION grest.address_list()
RETURNS TABLE (
  address varchar,
  address_hex text,
  script_address boolean,
  payment_cred text,
  stake_address text
)
LANGUAGE sql STABLE
AS $$
  SELECT
    a.address,
    ENCODE(a.raw, 'hex'),
    a.has_script,
    ENCODE(a.payment_cred, 'hex'),
    grest.cip5_hex_to_stake_addr(sa.hash_raw)
  FROM address AS a
  LEFT JOIN stake_address AS sa ON sa.id = a.stake_address_id
  ORDER BY a.id;
$$;

COMMENT ON FUNCTION grest.address_list IS 'Get a list of all addresses'; -- noqa: LT01

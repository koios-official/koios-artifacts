CREATE OR REPLACE FUNCTION grest.address_list()
RETURNS TABLE (
  address text,
  address_hex text,
  has_script boolean,
  payment_cred text,
  stake_address text,
  stake_address_hex text
)
LANGUAGE sql STABLE
AS $$
  SELECT
    a.address::text,
    ENCODE(a.raw,'hex')::text,
    a.has_script,
    ENCODE(a.payment_cred,'hex')::text,
    sa.view,
    ENCODE(sa.hash_raw,'hex')::text
  FROM address AS a
    LEFT JOIN stake_address AS sa ON a.stake_address_id = sa.id
  ORDER BY a.id DESC
  ;
$$;

COMMENT ON FUNCTION grest.address_list IS 'Get a list of all used addresses on chain'; -- noqa: LT01

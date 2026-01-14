CREATE OR REPLACE FUNCTION grest.asset_list(_asset_policy text DEFAULT NULL, _asset_name text DEFAULT NULL)
RETURNS TABLE (
  policy_id text,
  asset_name text,
  asset_name_ascii text,
  fingerprint text
)
LANGUAGE sql STABLE
AS $$
  SELECT
    ENCODE(ma.policy, 'hex')::text AS policy_id,
    ENCODE(ma.name, 'hex')::text AS asset_name,
    ENCODE(ma.name, 'escape')::text as asset_name_ascii,
    ma.fingerprint::text
  FROM public.multi_asset AS ma
  WHERE CASE WHEN _asset_policy IS NULL THEN TRUE ELSE ma.policy = DECODE(_asset_policy, 'hex') END
    AND CASE WHEN _asset_name IS NULL THEN TRUE ELSE ma.name = DECODE(_asset_name, 'hex') END
  ORDER BY ma.policy, ma.name;
$$;

COMMENT ON FUNCTION grest.asset_list IS 'Get a raw listing of all native assets on chain, without any CIP overlays'; --noqa: LT01

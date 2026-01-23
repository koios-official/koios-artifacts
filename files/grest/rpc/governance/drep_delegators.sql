CREATE OR REPLACE FUNCTION grest.drep_delegators(_drep_id text)
RETURNS TABLE (
  stake_address text,
  stake_address_hex text,
  script_hash text,
  epoch_no word31type,
  amount text
)
LANGUAGE plpgsql
AS $$
DECLARE
  drep_idx        bigint;
  last_reg_tx_id  bigint;
BEGIN

  IF STARTS_WITH(_drep_id,'drep_always') THEN
    -- predefined DRep roles
    SELECT INTO drep_idx id
    FROM public.drep_hash
    WHERE view = _drep_id;

    last_reg_tx_id := 0;
  ELSE
    SELECT INTO drep_idx id
    FROM public.drep_hash
    WHERE raw = DECODE((SELECT grest.cip129_drep_id_to_hex(_drep_id)), 'hex')
      AND has_script = grest.cip129_drep_id_has_script(_drep_id);

    SELECT INTO last_reg_tx_id COALESCE(MAX(tx_id), 0)
    FROM public.drep_registration
    WHERE drep_hash_id = drep_idx
      AND (deposit IS NOT NULL AND deposit >= 0);
  END IF;

  RETURN QUERY (
    WITH
      _all_delegations AS (
        SELECT 
          latest.addr_id,
          latest.tx_id,
          latest.drep_hash_id
        FROM (
          SELECT DISTINCT ON (dv.addr_id)
            dv.addr_id,
            dv.tx_id,
            dv.drep_hash_id
          FROM public.delegation_vote AS dv
          WHERE dv.tx_id >= last_reg_tx_id
          ORDER BY dv.addr_id, dv.tx_id DESC
        ) AS latest
        WHERE latest.drep_hash_id = drep_idx
          AND NOT EXISTS (
            SELECT 1
            FROM public.stake_deregistration sd 
            WHERE sd.addr_id = latest.addr_id 
              AND sd.tx_id > latest.tx_id
          )
      )

    SELECT
      grest.cip5_hex_to_stake_addr(sa.hash_raw)::text,
      ENCODE(sa.hash_raw,'hex'),
      ENCODE(sa.script_hash,'hex'),
      b.epoch_no,
      COALESCE(sdc.total_balance,0)::text
    FROM _all_delegations AS ad
      INNER JOIN stake_address AS sa ON ad.addr_id = sa.id
      INNER JOIN tx ON ad.tx_id = tx.id
      INNER JOIN block AS b ON tx.block_id = b.id
      LEFT JOIN grest.stake_distribution_cache AS sdc ON sa.id = sdc.stake_address_id
    ORDER BY b.epoch_no DESC, grest.cip5_hex_to_stake_addr(sa.hash_raw)
  );

END;
$$;

COMMENT ON FUNCTION grest.drep_delegators IS 'Return all delegators for a specific DRep'; -- noqa: LT01

CREATE OR REPLACE FUNCTION grest.pool_invalid_delegators(_pool_bech32 text, _epoch_no word31type)
RETURNS TABLE (
  stake_address text,
  epoch_no word31type
)
LANGUAGE plpgsql
AS $$
DECLARE
  _pool_id    bigint;
  _max_tx_id  bigint;
BEGIN
  SELECT id INTO _pool_id FROM public.pool_hash WHERE hash_raw = cardano.bech32_decode_data(_pool_bech32);
  
  SELECT MAX(id) INTO _max_tx_id 
  FROM public.tx 
  WHERE tx.block_id = (SELECT MAX(id) FROM public.block WHERE block.epoch_no = _epoch_no - 2);

  RETURN QUERY
    WITH _pool_specific_latest AS (
      SELECT DISTINCT ON (d.addr_id) 
        d.addr_id, 
        d.tx_id
      FROM public.delegation d
      WHERE d.pool_hash_id = _pool_id 
        AND d.tx_id <= _max_tx_id
      ORDER BY d.addr_id, d.tx_id DESC
    )
    SELECT
      grest.cip5_hex_to_stake_addr(sa.hash_raw)::text,
      _epoch_no
    FROM _pool_specific_latest psl
    JOIN public.stake_address sa ON psl.addr_id = sa.id
    WHERE 
      NOT EXISTS (
        SELECT 1 FROM public.delegation d_global
        WHERE d_global.addr_id = psl.addr_id
          AND d_global.tx_id > psl.tx_id
          AND d_global.tx_id <= _max_tx_id
      )
      AND NOT EXISTS (
        SELECT 1 FROM public.stake_deregistration sd
        WHERE sd.addr_id = psl.addr_id 
          AND sd.tx_id > psl.tx_id 
          AND sd.tx_id <= _max_tx_id
      )
      AND NOT EXISTS (
        SELECT 1 FROM public.epoch_stake es
        WHERE es.addr_id = psl.addr_id 
          AND es.epoch_no = _epoch_no
      );
END;
$$;

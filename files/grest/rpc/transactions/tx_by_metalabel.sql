CREATE OR REPLACE FUNCTION grest.tx_by_metalabel(_label word64type, _after_block_height integer DEFAULT 0)
RETURNS TABLE (
  tx_hash text,
  block_hash text,
  block_height word31type,
  epoch_no word31type,
  absolute_slot word63type,
  tx_timestamp integer
)
LANGUAGE sql STABLE
AS $$

  WITH tx_id_min as (
    SELECT MIN(id) as tx_id
    FROM tx
    WHERE block_id = (
      SELECT block_id
      FROM block
        INNER JOIN tx ON tx.block_id = block.id
      WHERE block.block_no >= _after_block_height
      ORDER BY block_no
      LIMIT 1
    )
  )

  SELECT
    ENCODE(tx.hash::bytea, 'hex'),
    ENCODE(block.hash, 'hex'),
    block.block_no,
    block.epoch_no,
    block.slot_no,
    EXTRACT(EPOCH FROM block.time)::integer
  FROM tx_metadata tm
    INNER JOIN tx_id_min tim ON tm.tx_id >= tim.tx_id
    INNER JOIN tx ON tx.id = tm.tx_id
    INNER JOIN block ON block.id = tx.block_id
  WHERE tm.key = _label;

$$;

COMMENT ON FUNCTION grest.tx_by_metalabel IS 'Get all transactions that contain metadata with specified label(key)'; -- noqa: LT01
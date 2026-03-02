CREATE TABLE IF NOT EXISTS grest.pool_history_cache (
  pool_id bigint,
  epoch_no int8 NULL,
  active_stake lovelace NULL,
  active_stake_pct numeric NULL,
  saturation_pct numeric NULL,
  block_cnt int8 NULL,
  delegator_cnt int8 NULL,
  pool_fee_variable float8 NULL,
  pool_fee_fixed lovelace NULL,
  pool_fees float8 NULL,
  deleg_rewards float8 NULL,
  member_rewards float8 NULL,
  epoch_ros numeric NULL,
  PRIMARY KEY (pool_id, epoch_no)
);

COMMENT ON TABLE grest.pool_history_cache IS 'A history of pool performance including blocks, delegators, active stake, fees AND rewards';

CREATE OR REPLACE FUNCTION grest.get_pool_history_data_bulk(_epoch_no_to_insert_from word31type, _pool_bech32 text [] DEFAULT null, _epoch_no_until word31type DEFAULT null)
RETURNS TABLE (
  pool_id bigint,
  epoch_no bigint,
  active_stake lovelace,
  active_stake_pct numeric,
  saturation_pct numeric,
  block_cnt bigint,
  delegator_cnt bigint,
  margin double precision,
  fixed_cost lovelace,
  pool_fees double precision,
  deleg_rewards double precision,
  member_rewards double precision,
  epoch_ros numeric
)
LANGUAGE plpgsql
AS $$
#variable_conflict use_column
DECLARE
  _pool_ids bigint [];
BEGIN
  _pool_ids := (SELECT ARRAY_AGG(id) from pool_hash ph where ph.hash_raw = ANY(
    SELECT cardano.bech32_decode_data(pool)
    FROM UNNEST(_pool_bech32) AS pool)
  );

  RETURN QUERY
  
  WITH
    epoch_constants AS (
      SELECT
        e.no AS epoch_no,
        ep.optimal_pool_count,
        (SELECT supply FROM grest.totals(e.no)) AS total_supply,
        easc.amount AS global_active_stake
      FROM epoch AS e
      JOIN epoch_param AS ep ON ep.epoch_no = e.no
      LEFT JOIN grest.epoch_active_stake_cache AS easc ON easc.epoch_no = e.no
      WHERE e.no >= _epoch_no_to_insert_from
        AND (_epoch_no_until IS NULL OR e.no <= _epoch_no_until)
    ),
    blockcounts AS (
      SELECT
        sl.pool_hash_id,
        b.epoch_no,
        COUNT(*) AS block_cnt
      FROM block AS b
        JOIN slot_leader AS sl ON b.slot_leader_id = sl.id
      WHERE (_pool_bech32 IS NULL OR sl.pool_hash_id = ANY(_pool_ids))
        AND b.epoch_no >= _epoch_no_to_insert_from
        AND (_epoch_no_until IS NULL OR b.epoch_no <= _epoch_no_until) 
      GROUP BY
        sl.pool_hash_id,
        b.epoch_no
    ),
    reward_totals AS (
      SELECT
        r.pool_id,
        r.earned_epoch,
        COALESCE(SUM(CASE WHEN r.type = 'leader' THEN r.amount ELSE 0 END), 0) AS leadertotal,
        COALESCE(SUM(CASE WHEN r.type = 'member' THEN r.amount ELSE 0 END), 0) AS memtotal
      FROM reward AS r
      WHERE (_pool_bech32 IS NULL OR r.pool_id = ANY(_pool_ids))
        AND r.earned_epoch >= _epoch_no_to_insert_from
        AND (_epoch_no_until IS NULL OR r.earned_epoch <= _epoch_no_until)
      GROUP BY r.pool_id, r.earned_epoch
    ),

    activeandfees AS (
      SELECT
        es.pool_id AS pool_id,
        es.epoch_no,
        SUM(es.amount) AS active_stake,
        COUNT(1) AS delegator_cnt,
        lpu.margin AS pool_fee_variable,
        lpu.fixed_cost AS pool_fee_fixed,
        (SUM(es.amount) / (
          SELECT NULLIF(easc.amount, 0)
          FROM grest.epoch_active_stake_cache AS easc
          WHERE easc.epoch_no = es.epoch_no
          )
        ) * 100 AS active_stake_pct,
        ROUND(
          (SUM(es.amount) / (
            SELECT supply::bigint / (
                SELECT ep.optimal_pool_count
                FROM epoch_param AS ep
                WHERE ep.epoch_no = es.epoch_no
              )
            FROM grest.totals (es.epoch_no)
            ) * 100
          ), 2
        ) AS saturation_pct
      FROM epoch_stake AS es
      LEFT JOIN LATERAL (
        SELECT pup.margin, pup.fixed_cost
        FROM pool_update pup
        WHERE pup.hash_id = es.pool_id
          AND pup.active_epoch_no <= es.epoch_no
        ORDER BY pup.id DESC
        LIMIT 1
      ) AS lpu ON TRUE # last_pool_update
      WHERE es.epoch_no >= _epoch_no_to_insert_from
        AND (_epoch_no_until IS NULL OR es.epoch_no < _epoch_no_until)
        AND (_pool_bech32 IS NULL OR es.pool_id = ANY(_pool_ids))
      GROUP BY es.pool_id, es.epoch_no
    )

  SELECT
    actf.pool_id::bigint,
    actf.epoch_no::bigint,
    actf.active_stake::lovelace,
    actf.active_stake_pct,
    actf.saturation_pct,
    COALESCE(b.block_cnt, 0) AS block_cnt,
    actf.delegator_cnt,
    actf.pool_fee_variable::double precision,
    actf.pool_fee_fixed,
    -- for debugging: rt.memtotal,
    -- for debugging: rt.leadertotal,
    CASE COALESCE(b.block_cnt, 0)
    WHEN 0 THEN
      0
    ELSE
      -- special CASE for WHEN reward information is not available yet
      CASE COALESCE(rt.leadertotal, 0) + COALESCE(rt.memtotal, 0)
        WHEN 0 THEN NULL
        ELSE
          CASE
            WHEN COALESCE(rt.leadertotal, 0) < actf.pool_fee_fixed THEN COALESCE(rt.leadertotal, 0)
            ELSE ROUND(actf.pool_fee_fixed + (((COALESCE(rt.memtotal, 0) + COALESCE(rt.leadertotal, 0)) - actf.pool_fee_fixed) * actf.pool_fee_variable))
          END
      END
    END AS pool_fees,
    CASE COALESCE(b.block_cnt, 0)
    WHEN 0 THEN
      0
    ELSE
      -- special CASE for WHEN reward information is not available yet
      CASE COALESCE(rt.leadertotal, 0) + COALESCE(rt.memtotal, 0)
        WHEN 0 THEN NULL
      ELSE
        CASE
          WHEN COALESCE(rt.leadertotal, 0) < actf.pool_fee_fixed THEN COALESCE(rt.memtotal, 0)
          ELSE ROUND(COALESCE(rt.memtotal, 0) + (COALESCE(rt.leadertotal, 0) - (actf.pool_fee_fixed + (((COALESCE(rt.memtotal, 0) + COALESCE(rt.leadertotal, 0)) - actf.pool_fee_fixed) * actf.pool_fee_variable))))
        END
      END
    END AS deleg_rewards,
    CASE COALESCE(b.block_cnt, 0)
      WHEN 0 THEN 0
    ELSE
      CASE COALESCE(rt.memtotal, 0)
        WHEN 0 THEN NULL
        ELSE COALESCE(rt.memtotal, 0)
      END
    END::double precision AS member_rewards,
    CASE COALESCE(b.block_cnt, 0)
      WHEN 0 THEN 0
    ELSE
      -- special CASE for WHEN reward information is not available yet
      CASE COALESCE(rt.leadertotal, 0) + COALESCE(rt.memtotal, 0)
        WHEN 0 THEN NULL
        ELSE
          CASE
            WHEN COALESCE(rt.leadertotal, 0) < actf.pool_fee_fixed THEN ROUND((((POW((LEAST(((COALESCE(rt.memtotal, 0)) / (NULLIF(actf.active_stake, 0))), 1000) + 1), 73) - 1)) * 100)::numeric, 9)
            -- using LEAST AS a way to prevent overflow, in CASE of dodgy database data (e.g. giant rewards / tiny active stake)
            ELSE ROUND((((POW((LEAST((((COALESCE(rt.memtotal, 0) + (COALESCE(rt.leadertotal, 0) - (actf.pool_fee_fixed + (((COALESCE(rt.memtotal, 0)
                + COALESCE(rt.leadertotal, 0)) - actf.pool_fee_fixed) * actf.pool_fee_variable))))) / (NULLIF(actf.active_stake, 0))), 1000) + 1), 73) - 1)) * 100)::numeric, 9)
          END
      END
    END AS epoch_ros
  FROM activeandfees AS actf
  LEFT JOIN blockcounts AS b ON actf.pool_id = b.pool_hash_id
    AND actf.epoch_no = b.epoch_no
  LEFT JOIN reward_totals AS rt ON actf.pool_id = rt.pool_id
    AND actf.epoch_no = rt.earned_epoch;
     
END;
$$;

COMMENT ON FUNCTION grest.get_pool_history_data_bulk IS 'Pool block production and reward history from a given epoch until optional later epoch, for all OR particular subset of pools'; -- noqa: LT01

CREATE OR REPLACE FUNCTION grest.pool_history_cache_update(_epoch_no_to_insert_from bigint DEFAULT null)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  _curr_epoch bigint;
  _latest_epoch_no_in_cache bigint;
BEGIN
  IF (
    SELECT COUNT(pid) > 1
    FROM pg_stat_activity
    WHERE state = 'active' AND query ILIKE '%grest.pool_history_cache_update%'
      AND datname = (SELECT current_database())
    ) THEN
      RAISE EXCEPTION 'Previous pool_history_cache_update query still running but should have completed! Exiting...';
  END IF;

  IF (
    SELECT COUNT(key) != 1
    FROM GREST.CONTROL_TABLE
    WHERE key = 'last_active_stake_validated_epoch'
  ) THEN
    RAISE EXCEPTION 'Active stake cache not yet populated! Exiting...';
  END IF;

  SELECT COALESCE(MAX(epoch_no), 0) INTO _latest_epoch_no_in_cache FROM grest.pool_history_cache;
  -- Split into 500 epochs at a time to avoid hours spent on a single query (which can be risky if that query is killed)
  SELECT LEAST( 500 , (MAX(no) - _latest_epoch_no_in_cache) ) + _latest_epoch_no_in_cache INTO _curr_epoch FROM epoch;

  IF _epoch_no_to_insert_from IS NULL THEN
    IF _latest_epoch_no_in_cache = 0 THEN
      RAISE NOTICE 'Pool history cache table is empty, starting initial population...';
      PERFORM grest.pool_history_cache_update (0);
      RETURN;
    END IF;
    -- no-op IF we already have data up until second most recent epoch
    IF _latest_epoch_no_in_cache >= (_curr_epoch - 2) THEN
      INSERT INTO grest.control_table (key, last_value)
        VALUES ('pool_history_cache_last_updated', NOW() AT TIME ZONE 'utc')
      ON CONFLICT (key)
        DO UPDATE SET last_value = NOW() AT TIME ZONE 'utc';
      RETURN;
    END IF;
    -- IF current epoch is at least 2 ahead of latest in cache, repopulate FROM latest in cache until current-1
    _epoch_no_to_insert_from := _latest_epoch_no_in_cache;
  END IF;
  -- purge the data for the given epoch range, in theory should do nothing IF invoked only at start of new epoch
  DELETE FROM grest.pool_history_cache
  WHERE epoch_no >= _epoch_no_to_insert_from;

  RAISE NOTICE 'inserting data from % to %', _epoch_no_to_insert_from, _curr_epoch;


  INSERT INTO grest.pool_history_cache (
    select * from grest.get_pool_history_data_bulk(_epoch_no_to_insert_from::word31type, null::text [], _curr_epoch::word31type)
  );

  INSERT INTO grest.control_table (key, last_value)
    VALUES ('pool_history_cache_last_updated', NOW() AT TIME ZONE 'utc')
  ON CONFLICT (key)
    DO UPDATE SET last_value = NOW() AT TIME ZONE 'utc';

END;
$$;

COMMENT ON FUNCTION grest.pool_history_cache_update IS 'Internal function to update pool history for data FROM specified epoch until current-epoch-minus-one. Invoke WITH non-empty param for initial population, WITH empty for subsequent updates'; --noqa: LT01

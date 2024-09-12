-- CIP References
-- 0005: Common bech32 prefixes https://cips.cardano.org/cip/CIP-0005
-- 0019: Cardano Addresses https://cips.cardano.org/cip/CIP-0019

CREATE OR REPLACE FUNCTION grest.cip5_hex_to_stake_addr(hex bytea)
RETURNS text
LANGUAGE plpgsql STABLE
AS $$
BEGIN
  IF SUBSTRING(hex from 2 for 1) = '0' THEN
    RETURN b32_encode('stake_test', hex::text);
  ELSE
    RETURN b32_encode('stake', hex::text);
  END IF;
END;
$$;
CREATE OR REPLACE FUNCTION grest.has_security_param(description jsonb)
RETURNS boolean
LANGUAGE plpgsql
AS $$
DECLARE

BEGIN

  RETURN jsonb_path_exists(description, '$.** ? (exists(@."maxBlockBodySize") || exists(@."maxTxSize") || exists(@."maxBlockHeaderSize") || exists(@."maxValueSize") || exists(@."maxBlockExecutionUnits") || exists(@."txFeePerByte") || exists(@."txFeeFixed") || exists(@."utxoCostPerByte") || exists(@."govActionDeposit") || exists(@."minFeeRefScriptCostPerByte"))');

END;
$$;

COMMENT ON FUNCTION grest.has_security_param IS 'Returns a boolean to indicate whether a given gov action proposal description contains at least one security parameter'; --noqa: LT01

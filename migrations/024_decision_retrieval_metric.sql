-- Keep retrieval evaluation history under a provider-independent operation name.
UPDATE op_metrics
SET op = 'decision_retrieval'
WHERE op = 'jev_retrieval';

ALTER TABLE benchmark_results
ADD COLUMN gpu_count INTEGER CHECK (gpu_count IS NULL OR gpu_count > 0);

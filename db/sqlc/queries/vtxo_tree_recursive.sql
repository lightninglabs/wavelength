-- VTXO tree recursive queries.
-- These queries support storing and retrieving VTXO trees in normalized form.

-- name: InsertVTXOTreeNode :exec
INSERT INTO vtxo_tree_nodes (
	round_id,
	batch_output_index,
	node_id,
	parent_node_id,
	parent_output_index,
	depth,
	is_leaf,
	input_hash,
	input_index,
	amount,
	signature,
	final_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: InsertVTXOTreeNodeOutput :exec
INSERT INTO vtxo_tree_node_outputs (
	round_id,
	batch_output_index,
	node_id,
	output_index,
	value,
	pk_script
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertVTXOTreeCosigner :exec
INSERT INTO vtxo_tree_cosigners (
	round_id,
	batch_output_index,
	node_id,
	cosigner_key,
	key_index
) VALUES ($1, $2, $3, $4, $5);

-- name: GetVTXOTreeNodes :many
SELECT
	node_id,
	parent_node_id,
	parent_output_index,
	depth,
	is_leaf,
	input_hash,
	input_index,
	amount,
	signature,
	final_key
FROM vtxo_tree_nodes
WHERE round_id = $1
  AND batch_output_index = $2
ORDER BY depth ASC, node_id ASC;

-- name: GetVTXOTreeNodeOutputs :many
SELECT
	node_id,
	output_index,
	value,
	pk_script
FROM vtxo_tree_node_outputs
WHERE round_id = $1
  AND batch_output_index = $2
ORDER BY node_id ASC, output_index ASC;

-- name: GetVTXOTreeCosigners :many
SELECT
	node_id,
	cosigner_key,
	key_index
FROM vtxo_tree_cosigners
WHERE round_id = $1
  AND batch_output_index = $2
ORDER BY node_id ASC, key_index ASC;

-- NOTE: Complex recursive queries with slices are commented out for now.
-- They can be implemented in Go code using multiple queries.
--
-- -- name: GetVTXOTreePathToLeaves :many
-- -- Recursive query to find all nodes on the path from root to specified leaf nodes.
-- WITH RECURSIVE tree_path AS (
-- 	SELECT n.node_id, n.parent_node_id, n.depth, n.is_leaf, n.amount
-- 	FROM vtxo_tree_nodes n
-- 	WHERE n.round_id = $1 AND n.batch_output_index = $2
-- 	  AND n.node_id IN (...)
-- 	UNION
-- 	SELECT n.node_id, n.parent_node_id, n.depth, n.is_leaf, n.amount
-- 	FROM vtxo_tree_nodes n
-- 	INNER JOIN tree_path tp ON n.node_id = tp.parent_node_id
-- )
-- SELECT DISTINCT node_id, parent_node_id, depth, is_leaf, amount
-- FROM tree_path ORDER BY depth ASC, node_id ASC;

-- name: GetVTXOTreeLeavesByCoSigner :many
-- Find all leaf nodes where a specific cosigner key is present.
-- This is useful for extracting paths relevant to a particular user.
SELECT DISTINCT
	n.node_id,
	n.parent_node_id,
	n.depth,
	n.amount,
	n.input_hash,
	n.input_index
FROM vtxo_tree_nodes n
INNER JOIN vtxo_tree_cosigners c
	ON n.round_id = c.round_id
	AND n.batch_output_index = c.batch_output_index
	AND n.node_id = c.node_id
WHERE n.round_id = $1
  AND n.batch_output_index = $2
  AND n.is_leaf = 1
  AND c.cosigner_key = $3
ORDER BY n.node_id ASC;

-- NOTE: This recursive query can be implemented in application code.
--
-- -- name: GetVTXOTreeSubtreeAmount :one
-- -- Get the total amount of all descendants of a given node using recursive CTE.
-- WITH RECURSIVE subtree AS (
-- 	SELECT node_id, amount, is_leaf FROM vtxo_tree_nodes
-- 	WHERE round_id = $1 AND batch_output_index = $2 AND node_id = $3
-- 	UNION ALL
-- 	SELECT n.node_id, n.amount, n.is_leaf
-- 	FROM vtxo_tree_nodes n INNER JOIN subtree st ON n.parent_node_id = st.node_id
-- )
-- SELECT COALESCE(SUM(amount), 0) as total_amount FROM subtree WHERE is_leaf = 1;

-- name: DeleteVTXOTreeRecursive :exec
-- Delete all recursive tree data for a given tree.
-- The cascading foreign keys will handle deletion of outputs and cosigners.
DELETE FROM vtxo_tree_nodes
WHERE round_id = $1
  AND batch_output_index = $2;

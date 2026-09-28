package ast

import (
	"fmt"
	"os"
	"sync"

	"github.com/dominikbraun/graph"
	"github.com/dominikbraun/graph/draw"
)

type TaskfileGraph struct {
	sync.Mutex
	graph.Graph[string, *TaskfileVertex]
}

// A TaskfileVertex is a vertex on the Taskfile DAG.
type TaskfileVertex struct {
	URI      string
	Taskfile *Taskfile
}

func taskfileHash(vertex *TaskfileVertex) string {
	return vertex.URI
}

func NewTaskfileGraph() *TaskfileGraph {
	return &TaskfileGraph{
		sync.Mutex{},
		graph.New(taskfileHash,
			graph.Directed(),
			graph.PreventCycles(),
			graph.Rooted(),
		),
	}
}

func (tfg *TaskfileGraph) Visualize(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return draw.DOT(tfg.Graph, f)
}

func (tfg *TaskfileGraph) Merge() (*Taskfile, error) {
	hashes, err := graph.TopologicalSort(tfg.Graph)
	if err != nil {
		return nil, err
	}

	predecessorMap, err := tfg.PredecessorMap()
	if err != nil {
		return nil, err
	}

	// Loop over each vertex in reverse topological order except for the root vertex.
	// This gives us a loop over every included Taskfile in an order which is safe to merge.
	for i := len(hashes) - 1; i > 0; i-- {
		hash := hashes[i]

		// Get the included vertex
		includedVertex, err := tfg.Vertex(hash)
		if err != nil {
			return nil, err
		}

		// Merge the included Taskfile into every Taskfile that includes it.
		// This is done one parent at a time: all of them read the included
		// Taskfile, and merging may modify it.
		for _, edge := range predecessorMap[hash] {
			// Get the base vertex
			vertex, err := tfg.Vertex(edge.Source)
			if err != nil {
				return nil, err
			}

			// Get the merge options
			includes, ok := edge.Properties.Data.([]*Include)
			if !ok {
				return nil, fmt.Errorf("task: Failed to get merge options")
			}

			// Merge the included Taskfiles into the parent Taskfile
			for _, include := range includes {
				if err := vertex.Taskfile.Merge(
					includedVertex.Taskfile,
					include,
				); err != nil {
					return nil, err
				}
			}
		}
	}

	// Get the root vertex
	rootVertex, err := tfg.Vertex(hashes[0])
	if err != nil {
		return nil, err
	}

	return rootVertex.Taskfile, nil
}

"use client";

import * as React from "react";

import { ExplorationGraph } from "@/components/exploration-graph";
import { api } from "@/lib/api";
import type { Edge, TaskNode } from "@/lib/types";

// FindingLineageView renders the exploration sub-graph from the task's initial
// 해당 발견 사항 노드까지 표시하며 작업 그래프와 같은 공격 경로 캔버스를 사용합니다.
// scoped to just this finding's lineage.
export function FindingLineageView({ findingId }: { findingId: string }) {
  const [nodes, setNodes] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let alive = true;
    api
      .findingLineage(findingId)
      .then((g) => {
        if (!alive) return;
        setNodes(g.nodes ?? []);
        setEdges(g.edges ?? []);
      })
      .catch(() => {})
      .finally(() => {
        if (alive) setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [findingId]);

  if (loaded && nodes.length === 0) {
    return (
      <p className="text-muted-foreground p-6 text-sm">
        표시할 경로 없음(탐색 노드와 연결되지 않았거나 소속 작업이 삭제됨).
      </p>
    );
  }

  return (
    <ExplorationGraph
      nodes={nodes}
      edges={edges}
      className="h-[68vh]"
      emptyHint={loaded ? "경로 없음" : "불러오는 중…"}
    />
  );
}

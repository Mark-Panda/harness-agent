// 个人记忆图谱：用 3D 视角按会话聚类展示事实、摘要与长期记忆，支持旋转、缩放与搜索。
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent } from "react";
import { Link } from "react-router-dom";
import { Badge, Button, Callout, Card, EmptyState, ErrorBanner, Input, LoadingState, PageHeader, cn } from "../components/ui";
import { requestJSON } from "../lib/api";
import { formatDateTime } from "../lib/format";
import { useRemote } from "../lib/hooks";
import type { MemoryGraph, MemoryGraphEdge, MemoryGraphNode, MemoryGraphNodeKind } from "../lib/types";

// 画布高度（CSS 像素），宽度跟随容器自适应。
const CANVAS_HEIGHT = 560;

// 默认视角：略微俯视并偏向一侧，让节点层次一眼可辨。
const DEFAULT_ROTATION = { yaw: -0.5, pitch: 0.32 };

// 节点类型的中文名称与配色。
const kindTheme: Record<MemoryGraphNodeKind, { label: string; color: string; dark: string; radius: number }> = {
  session: { label: "会话", color: "#c14a35", dark: "#7c2417", radius: 14 },
  fact: { label: "事实", color: "#3d8170", dark: "#1d4a3f", radius: 7 },
  summary: { label: "摘要", color: "#a17a24", dark: "#5d430e", radius: 8 },
  vector: { label: "长期记忆", color: "#6172b5", dark: "#33406f", radius: 6 },
};

// edgeTheme 按边类型给出颜色与线型，避免不同关系混在一起。
function edgeTheme(kind: string): { color: string; dashed: boolean } {
  switch (kind) {
    case "has_fact":
    case "from_fact":
      return { color: "#3d8170", dashed: false };
    case "has_summary":
    case "from_summary":
      return { color: "#a17a24", dashed: false };
    case "related":
      return { color: "#c14a35", dashed: true };
    default:
      return { color: "#8f8474", dashed: false };
  }
}

// springRest 返回不同关系边的理想长度，越紧密的关系越靠近。
function springRest(kind: string): number {
  switch (kind) {
    case "from_fact":
    case "from_summary":
      return 48;
    case "has_vector":
      return 66;
    case "related":
      return 120;
    default:
      return 82;
  }
}

interface Point3 {
  x: number;
  y: number;
  z: number;
}

interface ProjectedNode {
  id: string;
  x: number;
  y: number;
  radius: number;
  depth: number;
  opacity: number;
}

// hashString 生成稳定种子，保证同一份数据每次布局一致。
function hashString(value: string): number {
  let hash = 2166136261;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return hash >>> 0;
}

// mulberry32 返回确定性伪随机数生成器。
function mulberry32(seed: number): () => number {
  let state = seed;
  return () => {
    state += 0x6d2b79f5;
    let result = state;
    result = Math.imul(result ^ (result >>> 15), result | 1);
    result ^= result + Math.imul(result ^ (result >>> 7), result | 61);
    return ((result ^ (result >>> 14)) >>> 0) / 4294967296;
  };
}

// unitVector 用确定性随机数生成单位球面上的方向。
function unitVector(random: () => number): Point3 {
  const z = random() * 2 - 1;
  const angle = random() * Math.PI * 2;
  const radius = Math.sqrt(1 - z * z);
  return { x: Math.cos(angle) * radius, y: z, z: Math.sin(angle) * radius };
}

// layoutGraph3D 先按会话聚类撒点，再跑一轮轻量 3D 力导向，得到稳定的空间布局。
function layoutGraph3D(nodes: MemoryGraphNode[], edges: MemoryGraphEdge[]): Map<string, Point3> {
  const positions = new Map<string, Point3>();
  if (nodes.length === 0) {
    return positions;
  }
  const sessions = nodes.filter((node) => node.kind === "session");
  const others = nodes.filter((node) => node.kind !== "session");
  const sessionIDs = new Set(sessions.map((session) => session.id));
  const sessionPositions = new Map<string, Point3>();

  // 会话均匀分布在球面上，作为各自的聚类中心。
  sessions.forEach((session, index) => {
    if (sessions.length === 1) {
      sessionPositions.set(session.id, { x: 0, y: 0, z: 0 });
      return;
    }
    const offset = 2 / sessions.length;
    const y = index * offset - 1 + offset / 2;
    const ringRadius = Math.sqrt(Math.max(0, 1 - y * y));
    const angle = index * Math.PI * (3 - Math.sqrt(5));
    sessionPositions.set(session.id, {
      x: Math.cos(angle) * ringRadius * 240,
      y: y * 240,
      z: Math.sin(angle) * ringRadius * 240,
    });
  });

  for (const session of sessions) {
    positions.set(session.id, sessionPositions.get(session.id) ?? { x: 0, y: 0, z: 0 });
  }

  // 子节点围绕所属会话撒在半径 40–90 的球壳内。
  for (const node of others) {
    const random = mulberry32(hashString(node.id));
    const sessionNodeID = node.session_id ? `session:${node.session_id}` : "";
    const origin = sessionIDs.has(sessionNodeID) ? sessionPositions.get(sessionNodeID) : undefined;
    const direction = unitVector(random);
    const distance = origin ? 40 + random() * 50 : 110 + random() * 100;
    positions.set(node.id, {
      x: (origin?.x ?? 0) + direction.x * distance,
      y: (origin?.y ?? 0) + direction.y * distance,
      z: (origin?.z ?? 0) + direction.z * distance,
    });
  }

  // 轻量力导向：节点互斥、边弹簧收缩、整体轻微向心，避免节点重叠。
  const ids = Array.from(positions.keys());
  const coordinates = ids.map((id) => positions.get(id) ?? { x: 0, y: 0, z: 0 });
  const indexByID = new Map(ids.map((id, index) => [id, index]));
  const springs = edges
    .map((edge) => ({
      source: indexByID.get(edge.source),
      target: indexByID.get(edge.target),
      rest: springRest(edge.kind),
    }))
    .filter((spring): spring is { source: number; target: number; rest: number } =>
      spring.source !== undefined && spring.target !== undefined);
  const iterations = coordinates.length > 400 ? 55 : 95;

  for (let step = 0; step < iterations; step += 1) {
    const forces = coordinates.map(() => ({ x: 0, y: 0, z: 0 }));
    for (let first = 0; first < coordinates.length; first += 1) {
      const a = coordinates[first];
      for (let second = first + 1; second < coordinates.length; second += 1) {
        const b = coordinates[second];
        const dx = a.x - b.x;
        const dy = a.y - b.y;
        const dz = a.z - b.z;
        const distanceSquared = dx * dx + dy * dy + dz * dz;
        if (distanceSquared > 160000) {
          continue;
        }
        const distance = Math.sqrt(distanceSquared) + 0.01;
        const force = Math.min(30, 5200 / (distanceSquared + 60));
        const fx = (dx / distance) * force;
        const fy = (dy / distance) * force;
        const fz = (dz / distance) * force;
        forces[first].x += fx;
        forces[first].y += fy;
        forces[first].z += fz;
        forces[second].x -= fx;
        forces[second].y -= fy;
        forces[second].z -= fz;
      }
    }
    for (const spring of springs) {
      const a = coordinates[spring.source];
      const b = coordinates[spring.target];
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const dz = b.z - a.z;
      const distance = Math.sqrt(dx * dx + dy * dy + dz * dz) + 0.01;
      const pull = (distance - spring.rest) * 0.035;
      const fx = (dx / distance) * pull;
      const fy = (dy / distance) * pull;
      const fz = (dz / distance) * pull;
      forces[spring.source].x += fx;
      forces[spring.source].y += fy;
      forces[spring.source].z += fz;
      forces[spring.target].x -= fx;
      forces[spring.target].y -= fy;
      forces[spring.target].z -= fz;
    }
    for (let index = 0; index < coordinates.length; index += 1) {
      const point = coordinates[index];
      point.x += forces[index].x * 0.6 - point.x * 0.004;
      point.y += forces[index].y * 0.6 - point.y * 0.004;
      point.z += forces[index].z * 0.6 - point.z * 0.004;
      const length = Math.sqrt(point.x * point.x + point.y * point.y + point.z * point.z);
      if (length > 340) {
        const scale = 340 / length;
        point.x *= scale;
        point.y *= scale;
        point.z *= scale;
      }
    }
  }

  ids.forEach((id, index) => positions.set(id, coordinates[index]));
  return positions;
}

// projectPoint 把 3D 坐标旋转后做弱透视投影，返回屏幕坐标与深度系数。
function projectPoint(point: Point3, rotation: { yaw: number; pitch: number }) {
  const cosYaw = Math.cos(rotation.yaw);
  const sinYaw = Math.sin(rotation.yaw);
  const xAfterYaw = point.x * cosYaw - point.z * sinYaw;
  const zAfterYaw = point.x * sinYaw + point.z * cosYaw;
  const cosPitch = Math.cos(rotation.pitch);
  const sinPitch = Math.sin(rotation.pitch);
  const yAfterPitch = point.y * cosPitch - zAfterYaw * sinPitch;
  const depth = point.y * sinPitch + zAfterYaw * cosPitch;
  const perspective = 880;
  const scale = perspective / Math.max(240, perspective - depth);
  return { x: xAfterYaw, y: yAfterPitch, depth, scale };
}

// nodeRadius 按类型与权重计算节点半径。
function nodeRadius(node: MemoryGraphNode): number {
  const base = kindTheme[node.kind].radius;
  if (node.kind === "session") {
    return base + Math.min(8, node.weight * 2);
  }
  return base + Math.min(5, Math.floor(node.weight / 2));
}

// shortLabel 截断节点标题，避免文字相互覆盖。
function shortLabel(label: string): string {
  const runes = Array.from(label);
  return runes.length > 14 ? `${runes.slice(0, 14).join("")}…` : label;
}

// metadataRows 把节点元数据转成可读键值对。
function metadataRows(node: MemoryGraphNode): Array<[string, string]> {
  if (!node.metadata) {
    return [];
  }
  return Object.entries(node.metadata).map(([key, value]) => [key, typeof value === "string" ? value : JSON.stringify(value)]);
}

export default function MemoryGraphPage() {
  const graph = useRemote(() => requestJSON<MemoryGraph>("/api/v1/memory/graph"), []);
  const [query, setQuery] = useState("");
  const [hiddenKinds, setHiddenKinds] = useState<Set<MemoryGraphNodeKind>>(new Set());
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [hoveredID, setHoveredID] = useState<string | null>(null);
  const [rotation, setRotation] = useState(DEFAULT_ROTATION);
  const [zoom, setZoom] = useState(1);
  const [autoRotate, setAutoRotate] = useState(false);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const projectedRef = useRef<ProjectedNode[]>([]);
  const dragRef = useRef<{ x: number; y: number; moved: boolean } | null>(null);
  const suppressClickRef = useRef(false);

  const data = graph.data;
  const positions = useMemo(() => layoutGraph3D(data?.nodes ?? [], data?.edges ?? []), [data]);
  const normalizedQuery = query.trim().toLowerCase();
  const matched = useMemo(() => {
    if (!data || normalizedQuery === "") {
      return null;
    }
    return new Set(
      data.nodes
        .filter((node) => `${node.label} ${node.preview ?? ""}`.toLowerCase().includes(normalizedQuery))
        .map((node) => node.id),
    );
  }, [data, normalizedQuery]);

  const visibleNodes = useMemo(
    () => (data?.nodes ?? []).filter((node) => !hiddenKinds.has(node.kind)),
    [data, hiddenKinds],
  );
  const visibleIDs = useMemo(() => new Set(visibleNodes.map((node) => node.id)), [visibleNodes]);
  const visibleEdges = useMemo(
    () => (data?.edges ?? []).filter((edge) => visibleIDs.has(edge.source) && visibleIDs.has(edge.target)),
    [data, visibleIDs],
  );
  const selected = data?.nodes.find((node) => node.id === selectedID) ?? null;

  // 自动旋转：每一帧轻微改变偏航角，形成可感知的 3D 空间感。
  useEffect(() => {
    if (!autoRotate) {
      return;
    }
    let frame = 0;
    let previous = performance.now();
    const step = (now: number) => {
      const elapsed = Math.min(80, now - previous);
      previous = now;
      setRotation((current) => ({ ...current, yaw: current.yaw + elapsed * 0.00035 }));
      frame = window.requestAnimationFrame(step);
    };
    frame = window.requestAnimationFrame(step);
    return () => window.cancelAnimationFrame(frame);
  }, [autoRotate]);

  // 滚轮缩放需要非 passive 监听，才能在画布上滚动时阻止页面跟着滚动。
  useEffect(() => {
    const element = canvasRef.current;
    if (!element) {
      return;
    }
    const handler = (event: WheelEvent) => {
      event.preventDefault();
      setZoom((current) => Math.min(2.2, Math.max(0.55, current + (event.deltaY < 0 ? 0.08 : -0.08))));
    };
    element.addEventListener("wheel", handler, { passive: false });
    return () => element.removeEventListener("wheel", handler);
  }, []);

  // 绘制 3D 图谱：按深度排序，远小近大、远淡近实，形成纵深感。
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || !data) {
      return;
    }
    const context = canvas.getContext("2d");
    if (!context) {
      return;
    }
    const ratio = window.devicePixelRatio || 1;
    const width = canvas.clientWidth || 1;
    const height = canvas.clientHeight || CANVAS_HEIGHT;
    if (canvas.width !== Math.round(width * ratio) || canvas.height !== Math.round(height * ratio)) {
      canvas.width = Math.round(width * ratio);
      canvas.height = Math.round(height * ratio);
    }
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    context.clearRect(0, 0, width, height);

    const centerX = width / 2;
    const centerY = height / 2;
    // unit 控制整个球体的屏幕尺寸，zoom 只改变投影后的世界大小。
    const unit = Math.min(width, height) / 720;

    const projectedEdges = visibleEdges.flatMap((edge) => {
      const source = positions.get(edge.source);
      const target = positions.get(edge.target);
      if (!source || !target) {
        return [];
      }
      const from = projectPoint(source, rotation);
      const to = projectPoint(target, rotation);
      return [{
        edge,
        x1: centerX + from.x * unit * zoom * from.scale,
        y1: centerY + from.y * unit * zoom * from.scale,
        x2: centerX + to.x * unit * zoom * to.scale,
        y2: centerY + to.y * unit * zoom * to.scale,
        depth: (from.depth + to.depth) / 2,
        opacity: Math.min(0.75, 0.2 + (from.scale + to.scale) * 0.22),
      }];
    });

    const projectedNodes: ProjectedNode[] = [];
    for (const node of visibleNodes) {
      const point = positions.get(node.id);
      if (!point) {
        continue;
      }
      const projected = projectPoint(point, rotation);
      const base = nodeRadius(node);
      const x = centerX + projected.x * unit * zoom * projected.scale;
      const y = centerY + projected.y * unit * zoom * projected.scale;
      // 视野外的节点不参与绘制与命中，避免缩放后跑到画布外仍被点到。
      if (x < -80 || y < -80 || x > width + 80 || y > height + 80) {
        continue;
      }
      projectedNodes.push({
        id: node.id,
        x,
        y,
        radius: base * projected.scale,
        depth: projected.depth,
        opacity: Math.min(1, Math.max(0.35, 0.42 + (projected.scale - 0.7) * 1.5)),
      });
    }
    projectedNodes.sort((left, right) => left.depth - right.depth);

    // 先画远处的边，再画节点，保证近处节点覆盖远处连线。
    projectedEdges.sort((left, right) => left.depth - right.depth);
    for (const projected of projectedEdges) {
      const theme = edgeTheme(projected.edge.kind);
      const focused = selectedID === null || projected.edge.source === selectedID || projected.edge.target === selectedID;
      context.beginPath();
      context.setLineDash(theme.dashed ? [5, 4] : []);
      context.lineWidth = projected.edge.kind === "related" ? 1 : 1.4;
      context.strokeStyle = theme.color;
      context.globalAlpha = projected.opacity * (focused ? 1 : 0.25);
      context.moveTo(projected.x1, projected.y1);
      context.lineTo(projected.x2, projected.y2);
      context.stroke();
    }
    context.setLineDash([]);

    const nodeByID = new Map(visibleNodes.map((node) => [node.id, node]));
    for (const projected of projectedNodes) {
      const node = nodeByID.get(projected.id);
      if (!node) {
        continue;
      }
      const theme = kindTheme[node.kind];
      const dimmed = matched !== null && !matched.has(node.id);
      const isSelected = node.id === selectedID;
      const isHovered = node.id === hoveredID;
      const radius = projected.radius + (isSelected ? 4 : 0);
      context.globalAlpha = projected.opacity * (dimmed ? 0.22 : 1);

      // 径向渐变模拟球体受光，增强立体感。
      const gradient = context.createRadialGradient(
        projected.x - radius * 0.35,
        projected.y - radius * 0.4,
        radius * 0.1,
        projected.x,
        projected.y,
        radius,
      );
      gradient.addColorStop(0, theme.color);
      gradient.addColorStop(1, theme.dark);
      context.beginPath();
      context.arc(projected.x, projected.y, radius, 0, Math.PI * 2);
      context.fillStyle = gradient;
      context.fill();
      context.lineWidth = isSelected ? 2 : 1;
      context.strokeStyle = isSelected ? "#1f1b15" : "rgba(255,250,241,0.85)";
      context.stroke();

      if (node.kind === "session") {
        context.beginPath();
        context.arc(projected.x, projected.y, radius + 7, 0, Math.PI * 2);
        context.strokeStyle = theme.color;
        context.globalAlpha = 0.32 * (dimmed ? 0.3 : 1);
        context.lineWidth = 1;
        context.stroke();
      }

      const showLabel = node.kind === "session" || isSelected || isHovered || (matched !== null && matched.has(node.id));
      if (showLabel) {
        context.globalAlpha = dimmed ? 0.3 : 0.95;
        context.font = `${node.kind === "session" ? 13 : 11}px ui-sans-serif, system-ui, sans-serif`;
        context.textAlign = "center";
        context.textBaseline = "top";
        context.fillStyle = "#4a4237";
        context.fillText(shortLabel(node.label), projected.x, projected.y + radius + 6);
      }
    }
    context.globalAlpha = 1;
    projectedRef.current = projectedNodes;
  }, [data, visibleNodes, visibleEdges, positions, rotation, zoom, selectedID, hoveredID, matched]);

  const toggleKind = (kind: MemoryGraphNodeKind) => {
    setHiddenKinds((current) => {
      const next = new Set(current);
      if (next.has(kind)) {
        next.delete(kind);
      } else {
        next.add(kind);
      }
      return next;
    });
  };

  // resetView 恢复默认视角与缩放。
  const resetView = () => {
    setRotation(DEFAULT_ROTATION);
    setZoom(1);
  };

  // pickNode 在屏幕上寻找距离指针最近的节点，用于点击与悬停命中。
  const pickNode = (x: number, y: number): ProjectedNode | null => {
    let best: ProjectedNode | null = null;
    let bestDistance = Number.POSITIVE_INFINITY;
    for (const node of projectedRef.current) {
      const distance = Math.hypot(node.x - x, node.y - y);
      if (distance <= Math.max(node.radius + 8, 12) && distance < bestDistance) {
        best = node;
        bestDistance = distance;
      }
    }
    return best;
  };

  const pointerPosition = (event: ReactPointerEvent<HTMLCanvasElement>) => {
    const bounds = event.currentTarget.getBoundingClientRect();
    return { x: event.clientX - bounds.left, y: event.clientY - bounds.top };
  };

  const onPointerDown = (event: ReactPointerEvent<HTMLCanvasElement>) => {
    const point = pointerPosition(event);
    dragRef.current = { x: point.x, y: point.y, moved: false };
    suppressClickRef.current = false;
    event.currentTarget.setPointerCapture(event.pointerId);
  };

  const onPointerMove = (event: ReactPointerEvent<HTMLCanvasElement>) => {
    const point = pointerPosition(event);
    const drag = dragRef.current;
    if (drag) {
      const dx = point.x - drag.x;
      const dy = point.y - drag.y;
      if (Math.abs(dx) + Math.abs(dy) > 3) {
        drag.moved = true;
        suppressClickRef.current = true;
      }
      drag.x = point.x;
      drag.y = point.y;
      setRotation((current) => ({
        yaw: current.yaw + dx * 0.007,
        pitch: Math.min(1.25, Math.max(-1.25, current.pitch + dy * 0.006)),
      }));
      return;
    }
    const hit = pickNode(point.x, point.y);
    setHoveredID(hit?.id ?? null);
  };

  const onPointerUp = (event: ReactPointerEvent<HTMLCanvasElement>) => {
    const point = pointerPosition(event);
    const wasDragging = dragRef.current?.moved ?? false;
    dragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    if (wasDragging || suppressClickRef.current) {
      window.setTimeout(() => {
        suppressClickRef.current = false;
      }, 0);
      return;
    }
    const hit = pickNode(point.x, point.y);
    setSelectedID((current) => (hit ? (current === hit.id ? null : hit.id) : null));
  };

  const onPointerLeave = () => {
    if (!dragRef.current) {
      setHoveredID(null);
    }
  };

  const stats = data?.stats;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        eyebrow="个人记忆"
        title="记忆图谱"
        description="以 3D 视角把助手记住的会话摘要、结构化事实与长期记忆摊开来看：会话是空间中的聚类中心，连线表示记忆的来源与主题关联。"
        actions={
          <>
            <Link to="/sessions">
              <Button variant="secondary" size="sm">返回对话</Button>
            </Link>
            <Button size="sm" onClick={graph.reload} loading={graph.loading}>刷新图谱</Button>
          </>
        }
      />

      {graph.error ? <ErrorBanner message={graph.error} onRetry={graph.reload} /> : null}

      {graph.loading && !data ? <LoadingState label="正在组装记忆图谱" /> : null}

      {data && data.nodes.length === 0 ? (
        <EmptyState
          title="还没有形成记忆"
          description="先进行几轮对话，助手会在达到阈值后生成摘要、抽取事实并写入长期记忆；沉淀之后这里就会出现图谱。"
          action={<Link to="/sessions"><Button>去开始对话</Button></Link>}
        />
      ) : null}

      {data && data.nodes.length > 0 ? (
        <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_320px]">
          <Card
            title="3D 记忆关系图"
            description={`${data.nodes.length} 个节点 · ${data.edges.length} 条关系 · 生成于 ${formatDateTime(data.generated_at)}`}
            actions={
              <div className="flex flex-wrap items-center gap-2">
                <div className="w-48">
                  <Input
                    value={query}
                    onChange={(event) => setQuery(event.target.value)}
                    placeholder="搜索记忆内容"
                    aria-label="搜索记忆节点"
                  />
                </div>
                <Button
                  size="sm"
                  variant={autoRotate ? "primary" : "secondary"}
                  aria-pressed={autoRotate}
                  onClick={() => setAutoRotate((current) => !current)}
                >
                  {autoRotate ? "停止旋转" : "自动旋转"}
                </Button>
                <Button size="sm" variant="secondary" onClick={() => setZoom((current) => Math.max(0.55, current - 0.12))} aria-label="缩小">−</Button>
                <span className="min-w-10 text-center text-xs text-muted">{Math.round(zoom * 100)}%</span>
                <Button size="sm" variant="secondary" onClick={() => setZoom((current) => Math.min(2.2, current + 0.12))} aria-label="放大">＋</Button>
                <Button size="sm" variant="secondary" onClick={resetView}>重置视角</Button>
              </div>
            }
          >
            {stats?.truncated ? (
              <div className="mb-4">
                <Callout tone="warning" title="图谱已按容量截断">
                  为保证页面流畅，最多展示 200 个会话、100 条事实、200 条摘要与 200 条长期记忆。更早的记忆仍可通过导出查看。
                </Callout>
              </div>
            ) : null}
            <div className="mb-3 flex flex-wrap items-center gap-2">
              {(Object.keys(kindTheme) as MemoryGraphNodeKind[]).map((kind) => {
                const active = !hiddenKinds.has(kind);
                const count = kind === "session" ? stats?.sessions : kind === "fact" ? stats?.facts : kind === "summary" ? stats?.summaries : stats?.vectors;
                return (
                  <button
                    key={kind}
                    type="button"
                    aria-pressed={active}
                    onClick={() => toggleKind(kind)}
                    className={cn(
                      "inline-flex items-center gap-2 rounded-[2px] border px-2 py-1 text-[11px] transition",
                      active ? "border-line-strong bg-paper-raised text-ink-soft" : "border-line bg-paper-deep/40 text-muted opacity-70",
                    )}
                  >
                    <span aria-hidden className="size-2.5 rounded-full" style={{ backgroundColor: kindTheme[kind].color }} />
                    {kindTheme[kind].label}
                    <span className="font-mono text-[10px] text-muted">{count ?? 0}</span>
                  </button>
                );
              })}
              <span className="ml-auto text-[11px] text-muted">拖拽旋转 3D 视角 · 滚轮缩放 · 点击节点查看详情 · 虚线表示同主题关联</span>
            </div>
            <div className="relative overflow-hidden rounded-[3px] border border-line bg-[radial-gradient(circle_at_center,rgba(82,64,40,0.06),transparent_62%)]">
              <canvas
                ref={canvasRef}
                role="img"
                aria-label="个人记忆 3D 知识图谱"
                className={cn("block w-full touch-none select-none", hoveredID ? "cursor-pointer" : "cursor-grab active:cursor-grabbing")}
                style={{ height: `${CANVAS_HEIGHT}px` }}
                onPointerDown={onPointerDown}
                onPointerMove={onPointerMove}
                onPointerUp={onPointerUp}
                onPointerCancel={onPointerUp}
                onPointerLeave={onPointerLeave}
              />
              <div className="pointer-events-none absolute bottom-2 left-3 rounded bg-paper-raised/85 px-2 py-1 font-mono text-[10px] text-muted">
                视角 {Math.round((rotation.yaw * 180) / Math.PI)}° / {Math.round((rotation.pitch * 180) / Math.PI)}°
              </div>
            </div>
          </Card>

          <div className="flex flex-col gap-6">
            <Card title="节点详情" description={selected ? kindTheme[selected.kind].label : "点击图中节点查看记忆内容。"}>
              {selected ? (
                <div className="flex flex-col gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone={selected.kind === "session" ? "danger" : selected.kind === "fact" ? "success" : "warning"}>
                      {kindTheme[selected.kind].label}
                    </Badge>
                    <span className="break-all font-mono text-[11px] text-muted">{selected.id}</span>
                  </div>
                  <div>
                    <p className="text-sm font-medium text-ink">{selected.label}</p>
                    {selected.preview ? <p className="mt-1 whitespace-pre-wrap break-words text-xs leading-5 text-ink-soft">{selected.preview}</p> : null}
                  </div>
                  <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px] text-muted">
                    <dt>创建时间</dt>
                    <dd className="text-ink-soft">{formatDateTime(selected.created_at)}</dd>
                    {metadataRows(selected).map(([key, value]) => (
                      <Fragment key={key}>
                        <dt>{key}</dt>
                        <dd className="break-words text-ink-soft">{value}</dd>
                      </Fragment>
                    ))}
                  </dl>
                  {selected.session_id ? (
                    <Link to={`/sessions/${selected.session_id}`}>
                      <Button size="sm" variant="secondary">打开所属会话</Button>
                    </Link>
                  ) : null}
                </div>
              ) : (
                <p className="text-xs leading-5 text-muted">点击任意节点可查看记忆正文、来源类型与创建时间；会话节点可直接跳转到对话。</p>
              )}
            </Card>
            <Card title="图谱说明" description="3D 视角与记忆关系">
              <ul className="flex flex-col gap-2 text-xs leading-5 text-ink-soft">
                <li><span className="font-medium text-ink">空间位置</span>：同一会话的记忆聚成一个簇，节点远近体现空间层次。</li>
                <li><span className="font-medium text-ink">实线</span>：记忆归属于会话，或由摘要、事实向量化而来。</li>
                <li><span className="font-medium text-ink">虚线</span>：不同事实共享同一主题前缀（如 profile.*）。</li>
                <li>图谱只读取当前账号的记忆，不会包含其他用户的数据。</li>
              </ul>
            </Card>
          </div>
        </div>
      ) : null}
    </div>
  );
}

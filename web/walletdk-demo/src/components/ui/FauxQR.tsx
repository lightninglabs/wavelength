// FauxQR renders a deterministic, decorative QR-like matrix. It is not a real
// QR code - it only exists to give the receive screens a believable visual
// until a real encoder is wired in.

// cells derives a deterministic on/off matrix from a seed string.
function cells(seed: string, size: number): boolean[] {
  const out: boolean[] = [];
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  for (let i = 0; i < size * size; i++) {
    h ^= h << 13;
    h ^= h >>> 17;
    h ^= h << 5;
    out.push((h & 1) === 1);
  }

  return out;
}

export function FauxQR({
  seed = "walletdk",
  size = 21,
  className = "",
  color = "currentColor",
}: {
  seed?: string;
  size?: number;
  className?: string;
  color?: string;
}) {
  const matrix = cells(seed, size);
  const finder = (x: number, y: number) =>
    (x < 7 && y < 7) || (x >= size - 7 && y < 7) || (x < 7 && y >= size - 7);

  return (
    <svg
      viewBox={`0 0 ${size} ${size}`}
      className={className}
      shapeRendering="crispEdges"
      aria-hidden="true"
    >
      {matrix.map((on, i) => {
        const x = i % size;
        const y = Math.floor(i / size);
        if (finder(x, y) || !on) {
          return null;
        }

        return <rect key={i} x={x} y={y} width="1" height="1" fill={color} />;
      })}
      {[
        [0, 0],
        [size - 7, 0],
        [0, size - 7],
      ].map(([fx, fy], idx) => (
        <g key={idx} fill={color}>
          <rect x={fx} y={fy} width="7" height="1" />
          <rect x={fx} y={fy + 6} width="7" height="1" />
          <rect x={fx} y={fy} width="1" height="7" />
          <rect x={fx + 6} y={fy} width="1" height="7" />
          <rect x={fx + 2} y={fy + 2} width="3" height="3" />
        </g>
      ))}
    </svg>
  );
}

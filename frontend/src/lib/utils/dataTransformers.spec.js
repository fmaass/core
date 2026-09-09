import { describe, expect, it } from 'vitest';
import {
  addPreservationTransitions,
  createEdge,
  edgesToTransitions,
  resolveConnectionDirection,
  transitionsToEdges,
} from './dataTransformers.js';

// The connection Svelte Flow actually reported for a drag from status-1's right
// side to status-2, recorded from the running designer (INFRA-60): the grab
// resolved to status-1's TARGET handle, so the connection arrives reversed.
const REPORTED_FOR_DRAG_1_TO_2 = {
  source: 'status-2',
  sourceHandle: 'target-left',
  target: 'status-1',
  targetHandle: 'target-right',
};

// The heuristic that stood in onConnect before the fix, reproduced verbatim as
// the negative control: it swaps only when the DROP end is a source handle.
function legacyDirection(params) {
  let { source, target, sourceHandle, targetHandle } = params;
  const fromIsTarget = sourceHandle?.startsWith('target') || sourceHandle === 'target';
  const toIsSource =
    targetHandle?.startsWith('target') === false &&
    targetHandle !== undefined &&
    targetHandle !== null;
  if (fromIsTarget && toIsSource) {
    [source, target] = [target, source];
    [sourceHandle, targetHandle] = [targetHandle, sourceHandle];
  }
  return { source, target, sourceHandle, targetHandle };
}

describe('resolveConnectionDirection', () => {
  it('keeps the direction the pointer drew when the drag started on status-1', () => {
    const out = resolveConnectionDirection(REPORTED_FOR_DRAG_1_TO_2, 'status-1');
    expect(out.source).toBe('status-1');
    expect(out.target).toBe('status-2');
  });

  it('keeps the direction the pointer drew for the opposite drag', () => {
    const reportedForDrag2to1 = {
      source: 'status-1',
      sourceHandle: 'target-right',
      target: 'status-2',
      targetHandle: 'target-left',
    };
    const out = resolveConnectionDirection(reportedForDrag2to1, 'status-2');
    expect(out.source).toBe('status-2');
    expect(out.target).toBe('status-1');
  });

  it('is not a blanket swap: an already-correct report is left alone', () => {
    const alreadyCorrect = {
      source: 'status-1',
      sourceHandle: 'right',
      target: 'status-2',
      targetHandle: 'target-left',
    };
    expect(resolveConnectionDirection(alreadyCorrect, 'status-1')).toEqual(alreadyCorrect);
  });

  it('leaves the report untouched when no start node was recorded', () => {
    expect(resolveConnectionDirection(REPORTED_FOR_DRAG_1_TO_2, null)).toEqual(
      REPORTED_FOR_DRAG_1_TO_2
    );
  });

  it('swaps the handles with the ends', () => {
    const out = resolveConnectionDirection(REPORTED_FOR_DRAG_1_TO_2, 'status-1');
    expect(out.sourceHandle).toBe('target-right');
    expect(out.targetHandle).toBe('target-left');
  });

  it('negative control: the pre-fix heuristic inverts this very drag', () => {
    const legacy = legacyDirection(REPORTED_FOR_DRAG_1_TO_2);
    expect(legacy.source).toBe('status-2');
    expect(legacy.target).toBe('status-1');
  });

  it('a drag from A to B persists A->B end to end', () => {
    const { source, target } = resolveConnectionDirection(REPORTED_FOR_DRAG_1_TO_2, 'status-1');
    const edge = createEdge(
      Number.parseInt(source.replace('status-', ''), 10),
      Number.parseInt(target.replace('status-', ''), 10),
      7
    );
    expect(edge.id).toBe('edge-1-2');
    const [transition] = edgesToTransitions([edge], 7);
    expect(transition.from_status_id).toBe(1);
    expect(transition.to_status_id).toBe(2);
  });
});

describe('addPreservationTransitions', () => {
  // A status is a member of a workflow only by appearing in a transition:
  // loadWorkflowData() rebuilds the canvas from workflow.transitions alone and
  // there is no workflow_statuses table. The self-referencing row is that
  // membership marker; both loadWorkflowData and transitionsToEdges filter it
  // out, so it never draws an edge.
  const statuses = [{ id: 1 }, { id: 2 }, { id: 3 }];

  it('marks a status that appears in no transition', () => {
    const out = addPreservationTransitions(statuses, [{ from_status_id: 1, to_status_id: 2 }], 7);
    expect(out).toHaveLength(2);
    expect(out[1]).toMatchObject({ workflow_id: 7, from_status_id: 3, to_status_id: 3 });
  });

  it('leaves a connected status alone', () => {
    const out = addPreservationTransitions(
      statuses,
      [
        { from_status_id: 1, to_status_id: 2 },
        { from_status_id: 2, to_status_id: 3 },
      ],
      7
    );
    expect(out).toHaveLength(2);
  });

  it('counts the initial transition (from_status_id null) as membership', () => {
    const out = addPreservationTransitions([{ id: 9 }], [{ from_status_id: null, to_status_id: 9 }], 7);
    expect(out).toHaveLength(1);
  });

  it('the marker never reaches the canvas as an edge', () => {
    const withMarkers = addPreservationTransitions(statuses, [], 7);
    expect(transitionsToEdges(withMarkers)).toHaveLength(0);
  });
});

describe('handle ids survive a load/save round trip', () => {
  it('reads a stored target handle back onto a real target handle id', () => {
    const [edge] = transitionsToEdges([
      {
        id: 4,
        workflow_id: 7,
        from_status_id: 1,
        to_status_id: 2,
        source_handle: 'right',
        target_handle: 'target-left',
        display_order: 0,
      },
    ]);
    expect(edge.sourceHandle).toBe('right');
    expect(edge.targetHandle).toBe('target-left');
    const [transition] = edgesToTransitions([edge], 7);
    expect(transition.source_handle).toBe('right');
    expect(transition.target_handle).toBe('target-left');
  });
});

<script>
  import { currentRoute } from '../router.js';
  import { workspacePermissions } from '../stores';
  import { workspaceSettingsItems, workspaceSettingsRoute } from '../navigation/workspaceNavigation.js';
  import { navItemStyle, onNavMouseEnter, onNavMouseLeave } from '../navigation/navItemStyle.js';
  import { t } from '../stores/i18n.svelte.js';

  let { workspaceId = null } = $props();

  const canAdmin = $derived(workspacePermissions.canAdminWorkspace(workspaceId));
</script>

{#if canAdmin}
  <nav class="flex-1 px-4 pt-2 space-y-1 overflow-y-auto" data-testid="workspace-admin-nav" aria-label={t('workspaceSettings.title')}>
    {#each workspaceSettingsItems as item (item.id)}
      {@const ItemIcon = item.icon}
      {@const active = $currentRoute.view === item.view}
      <a
        href={workspaceSettingsRoute(workspaceId, item.id)}
        class="w-full text-left cursor-pointer px-3 py-2 rounded-lg text-sm font-medium flex items-center gap-2 workspace-nav-item no-underline"
        style={navItemStyle(active, item.danger)}
        data-testid="workspace-admin-nav-{item.id}"
        aria-current={active ? 'page' : undefined}
        onmouseenter={(e) => onNavMouseEnter(e, active, item.danger)}
        onmouseleave={(e) => onNavMouseLeave(e, active, item.danger)}
      >
        <span class="workspace-nav-item-content flex items-center gap-2 min-w-0">
          <ItemIcon class="w-4 h-4" />
          {t(item.labelKey)}
        </span>
      </a>
    {/each}
  </nav>
{/if}

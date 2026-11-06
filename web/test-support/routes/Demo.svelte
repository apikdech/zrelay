<script lang="ts">
  let { clipId = "" }: { clipId?: string } = $props();
</script>

<div data-testid="route-demo" data-clip={clipId}></div>

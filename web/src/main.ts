import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';
import { registerServiceWorker } from './lib/pwa/register';

mount(App, { target: document.getElementById('app')!, props: { updater: registerServiceWorker() } });

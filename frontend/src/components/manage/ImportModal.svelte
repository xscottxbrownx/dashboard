<div class="modal" transition:fade>
  <div class="modal-wrapper">
    <Card footer={true} footerRight={true} fill={false}>
      <span slot="title">Import Data</span>

      <div slot="body" class="body-wrapper">
        Please upload your exported data file to import settings and transcripts.

        <form>
          <div class="row">
            <div class="label-wrapper">
              <label for="import_data" class="form-label"> Data Export .zip</label>
            </div>
            <input type="file" id="import_data" style="display: block; width: 100%;" accept=".zip" />
          </div>
          <div class="row">
            <div class="label-wrapper">
              <label for="import_data" class="form-label"> Transcripts Export .zip</label>
            </div>
            <input type="file" id="import_transcripts" style="display: block; width: 100%;" accept=".zip" />
          </div>
        </form>
        {#if queryLoading}
          <div>
            <br />
            <br />
            <p style="text-align: center;">We are currently loading your data in, please do not navigate away from this page.</p>
          </div>
        {/if}
      </div>

      <div slot="footer" class="footer-wrapper">
        <Button danger={true} on:click={dispatchClose}>Cancel</Button>
        <div style="">
          <Button on:click={dispatchConfirm} disabled={queryLoading}>Confirm</Button>
        </div>
      </div>
    </Card>
  </div>
</div>

<div class="modal-backdrop" transition:fade></div>

<svelte:window on:keydown={handleKeydown}/>

<script>
    import {createEventDispatcher} from 'svelte';
    import {fade} from 'svelte/transition'
    import Card from "../Card.svelte";
    import Button from "../Button.svelte";

    import Textarea from '../form/Textarea.svelte';
    
    import {setDefaultHeaders} from '../../includes/Auth.svelte'
    import {notifyError, notifySuccess} from "../../js/util";
    import axios from "axios";
    import {API_URL} from "../../js/constants";
    setDefaultHeaders();

    export let guildId;

    let publicKey = "";

    let queryLoading = false;

    const dispatch = createEventDispatcher();

    function dispatchClose() {
        dispatch('close', {});
    }

    async function dispatchConfirm() {
      let dataFileInput = document.getElementById('import_data');

      let transcriptFileInput = document.getElementById('import_transcripts');

      if(dataFileInput.files.length === 0 && transcriptFileInput.files.length === 0) {
        notifyError('Please select a file to import, at least one of data or transcripts must be provided');
        return;
      }


        const frmData = new FormData();
        if (dataFileInput.files.length > 0) {
          frmData.append('data_file', dataFileInput.files[0]);
        }
        if (transcriptFileInput.files.length > 0) {
          frmData.append('transcripts_file', transcriptFileInput.files[0]);
        }

        queryLoading = true;
        const res = await axios.post(`${API_URL}/api/${guildId}/import`, frmData, {
          headers: {
            'Content-Type': 'multipart/form-data'
          }
        });
        if (res.status !== 200) {
            notifyError(`Failed to import settings: ${res.data.error}`);
            queryLoading = false;
            return;
        }
        queryLoading = false;

        dispatchClose();
        notifySuccess('Imported settings successfully - Your transcripts will be processed separately and may take some time to appear.');
    }

    function handleKeydown(e) {
        if (e.key === "Escape") {
            dispatchClose();
        }
    }
</script>

<style>
    .modal {
        position: absolute;
        top: 0;
        left: 0;
        width: 100%;
        height: 100%;
        z-index: 501;

        display: flex;
        justify-content: center;
        align-items: center;
    }

    .modal-wrapper {
        display: flex;
        width: 40%;
    }

    @media only screen and (max-width: 1280px) {
        .modal-wrapper {
            width: 96%;
        }
    }

    .modal-backdrop {
        position: fixed;
        top: 0;
        left: 0;
        width: 100%;
        height: 100%;
        z-index: 500;
        background-color: #000;
        opacity: .5;
    }

    .body-wrapper {
        display: flex;
        flex-direction: column;
        gap: 4px;
    }

    .footer-wrapper {
        display: flex;
        flex-direction: row;
        gap: 12px;
    }
</style>
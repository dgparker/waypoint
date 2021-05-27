import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { Deployment } from 'waypoint-pb';
import { action } from '@ember/object';
export default class WorkspaceProjectsProjectAppDeployments extends Controller {
  queryParams = ['destroyed'];

  @tracked destroyed = false;

  get hasMoreDeployments(): boolean {
    return this.model.filter((deployment: Deployment.AsObject) => deployment.state == 4).length > 0;
  }

  get deployments(): Deployment.AsObject[] {
    if (this.destroyed) {
      return this.model;
    } else {
      let deploys = this.model.filter((deployment: Deployment.AsObject) => deployment.state != 4);
      return deploys;
    }
  }

  @action
  showDestroyed(): void {
    this.destroyed = true;
  }
}
